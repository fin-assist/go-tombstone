package tombstone

import (
	"context"
	"errors"
	"net"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	assistant "github.com/fin-assist/contracts/gen/go/finassist/assistant/v1"
	user "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	deletion "github.com/fin-assist/contracts/gen/go/finassist/userdeletion/v1"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/fin-assist/go-identity"
	"google.golang.org/grpc/metadata"

	"github.com/fin-assist/go-tombstone/internal/pgtest"
)

type txKey struct{}
type testDB struct{ pool *pgxpool.Pool }

func (d testDB) Tx(ctx context.Context) (pgx.Tx, bool) {
	tx, ok := ctx.Value(txKey{}).(pgx.Tx)
	return tx, ok
}
func (d testDB) Within(ctx context.Context, fn func(context.Context) error) error {
	tx, err := d.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(context.WithValue(ctx, txKey{}, tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func setup(t *testing.T) (context.Context, testDB, *Store) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	dsn := pgtest.DSN(t)
	pgtest.Apply(t, dsn, MigrationsTable, Migrations)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err = pool.Exec(ctx, "CREATE TABLE service_data (user_id text PRIMARY KEY, value text NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	db := testDB{pool}
	s, err := New(db, pool, func() time.Time { return time.Unix(123, 0) })
	if err != nil {
		t.Fatal(err)
	}
	return ctx, db, s
}
func deleteData(db testDB, id string) func(context.Context) error {
	return func(ctx context.Context) error {
		tx, _ := db.Tx(ctx)
		_, err := tx.Exec(ctx, "DELETE FROM service_data WHERE user_id=$1", id)
		return err
	}
}
func writeData(ctx context.Context, db testDB, s *Store, id string) error {
	alive, err := s.Guard(ctx, id)
	if err != nil {
		return err
	}
	if !alive {
		return nil
	}
	tx, _ := db.Tx(ctx)
	_, err = tx.Exec(ctx, "INSERT INTO service_data VALUES ($1,'value') ON CONFLICT DO NOTHING", id)
	return err
}
func TestPostgresPurgeReplayRollbackAndSystemEvent(t *testing.T) {
	ctx, db, s := setup(t)
	if err := db.Within(ctx, func(ctx context.Context) error { return writeData(ctx, db, s, testUser) }); err != nil {
		t.Fatal(err)
	}
	if err := db.Within(ctx, func(ctx context.Context) error { return writeData(ctx, db, s, otherUser) }); err != nil {
		t.Fatal(err)
	}
	fail := errors.New("callback failed")
	err := db.Within(ctx, func(ctx context.Context) error {
		return s.Purge(ctx, testUser, func(ctx context.Context, id string) error {
			if err := deleteData(db, id)(ctx); err != nil {
				return err
			}
			return fail
		})
	})
	if !errors.Is(err, fail) {
		t.Fatal(err)
	}
	assertRolledBack := func() {
		t.Helper()
		deleted, err := s.Deleted(ctx, testUser)
		if err != nil || deleted {
			t.Fatalf("rollback tombstone %v %v", deleted, err)
		}
		var count int
		if err := db.pool.QueryRow(ctx, "SELECT count(*) FROM service_data WHERE user_id=$1", testUser).Scan(&count); err != nil || count != 1 {
			t.Fatalf("rollback data rows %d %v", count, err)
		}
	}
	assertRolledBack()
	// DeleteAccount may fail after a successful Purge; both its effects must roll back.
	err = db.Within(ctx, func(ctx context.Context) error {
		if err := s.Purge(ctx, testUser, func(ctx context.Context, id string) error { return deleteData(db, id)(ctx) }); err != nil {
			return err
		}
		tx, _ := db.Tx(ctx)
		var marked bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM tombstone_users WHERE user_id=$1)", testUser).Scan(&marked); err != nil {
			return err
		}
		if !marked {
			t.Error("Purge did not mark the user inside the transaction")
		}
		return fail
	})
	if !errors.Is(err, fail) {
		t.Fatal(err)
	}
	assertRolledBack()
	for i := 0; i < 2; i++ {
		if err := db.Within(ctx, func(ctx context.Context) error {
			return s.Purge(ctx, testUser, func(ctx context.Context, id string) error {
				if i > 0 {
					t.Fatal("replay callback")
				}
				return deleteData(db, id)(ctx)
			})
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Within(ctx, func(ctx context.Context) error { return writeData(ctx, db, s, testUser) }); err != nil {
		t.Fatal(err)
	}
	var deletedRows, foreignRows int
	if err := db.pool.QueryRow(ctx, "SELECT count(*) FILTER (WHERE user_id=$1), count(*) FILTER (WHERE user_id=$2) FROM service_data", testUser, otherUser).Scan(&deletedRows, &foreignRows); err != nil || deletedRows != 0 || foreignRows != 1 {
		t.Fatalf("deleted user rows %d, foreign user rows %d: %v", deletedRows, foreignRows, err)
	}
	var deletedAt time.Time
	if err := db.pool.QueryRow(ctx, "SELECT deleted_at FROM tombstone_users WHERE user_id='019cf000-0000-7000-8000-000000000001'").Scan(&deletedAt); err != nil || !deletedAt.Equal(time.Unix(123, 0)) {
		t.Fatalf("clock %v %v", deletedAt, err)
	}
	for _, iso := range []pgx.TxIsoLevel{pgx.RepeatableRead, pgx.Serializable, pgx.ReadUncommitted} {
		tx, err := db.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: iso})
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.Guard(context.WithValue(ctx, txKey{}, tx), testUser)
		if !errors.Is(err, ErrIsolation) {
			t.Fatalf("%s: %v", iso, err)
		}
		_ = tx.Rollback(ctx)
	}
}

func TestConcurrentWritesCannotResurrect(t *testing.T) {
	for _, writerFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "write first", false: "purge first"}[writerFirst], func(t *testing.T) {
			ctx, db, s := setup(t)
			tx, err := db.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			firstCtx := context.WithValue(ctx, txKey{}, tx)
			if writerFirst {
				err = writeData(firstCtx, db, s, testUser)
			} else {
				err = s.Purge(firstCtx, testUser, func(ctx context.Context, id string) error { return deleteData(db, id)(ctx) })
			}
			if err != nil {
				t.Fatal(err)
			}
			ready := make(chan uint32)
			done := make(chan error, 1)
			go func() {
				done <- db.Within(ctx, func(ctx context.Context) error {
					tx, _ := db.Tx(ctx)
					ready <- tx.Conn().PgConn().PID()
					if writerFirst {
						return s.Purge(ctx, testUser, func(ctx context.Context, id string) error { return deleteData(db, id)(ctx) })
					}
					return writeData(ctx, db, s, testUser)
				})
			}()
			pid := <-ready
			// Observe real contention before commit: this test cannot pass just because
			// the scheduler ran the second transaction after deletion completed.
			waitForAdvisory(t, ctx, db.pool, pid)
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			var count int
			if err := db.pool.QueryRow(ctx, "SELECT count(*) FROM service_data").Scan(&count); err != nil || count != 0 {
				t.Fatalf("resurrection %d %v", count, err)
			}
			deleted, err := s.Deleted(ctx, testUser)
			if err != nil || !deleted {
				t.Fatalf("tombstone %v %v", deleted, err)
			}
		})
	}
}

type profileServer struct {
	user.UnimplementedProfileServiceServer
	called *bool
}

func (s profileServer) GetProfile(context.Context, *user.GetProfileRequest) (*user.GetProfileResponse, error) {
	*s.called = true
	return &user.GetProfileResponse{}, nil
}
func TestServiceUsageOverGRPC(t *testing.T) {
	ctx, db, s := setup(t)
	auth, err := identity.NewServer(identity.AccessTable{
		user.ProfileService_GetProfile_FullMethodName:             {"bank-bff": identity.User},
		deletion.UserDeletionService_PurgeUserData_FullMethodName: {"user": identity.System},
		assistant.AssistantService_WatchAnswer_FullMethodName:     {"bank-bff": identity.User},
	}, serviceChecker{}, userChecker{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	unary, stream, err := Interceptors(s, func(ctx context.Context) string { id, _ := identity.UserID(ctx); return id }, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewDeletionServer(db, s, func(ctx context.Context, id string) error { return deleteData(db, id)(ctx) })
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1024 * 1024)
	t.Cleanup(func() { _ = listener.Close() })
	server := grpc.NewServer(grpc.ChainUnaryInterceptor(auth.Unary(), unary), grpc.ChainStreamInterceptor(auth.Stream(), stream))
	t.Cleanup(server.Stop)
	deletion.RegisterUserDeletionServiceServer(server, handler)
	called := false
	user.RegisterProfileServiceServer(server, profileServer{called: &called})
	streamCalled := false
	assistant.RegisterAssistantServiceServer(server, answerServer{called: &streamCalled})
	go func() { _ = server.Serve(listener) }()
	conn, err := grpc.NewClient("passthrough:///test", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := user.NewProfileServiceClient(conn)
	userCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs("x-service-authorization", "Bearer bank-bff", "authorization", "Bearer valid-user"))
	systemCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs("x-service-authorization", "Bearer user"))
	if _, err := client.GetProfile(ctx, &user.GetProfileRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("missing identity %v", err)
	}
	if _, err := client.GetProfile(userCtx, &user.GetProfileRequest{}); err != nil || !called {
		t.Fatalf("live %v %v", called, err)
	}
	ac := assistant.NewAssistantServiceClient(conn)
	liveStream, err := ac.WatchAnswer(userCtx, &assistant.WatchAnswerRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := liveStream.Recv(); err != nil || !streamCalled {
		t.Fatalf("live stream %v %v", streamCalled, err)
	}
	dc := deletion.NewUserDeletionServiceClient(conn)
	if _, err := dc.PurgeUserData(userCtx, &deletion.PurgeUserDataRequest{UserId: testUser}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("foreign caller %v", err)
	}
	if deleted, err := s.Deleted(ctx, testUser); err != nil || deleted {
		t.Fatalf("unauthorized purge took effect %v %v", deleted, err)
	}
	if err := db.Within(ctx, func(ctx context.Context) error { return writeData(ctx, db, s, testUser) }); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		id := testUser
		if i == 0 {
			id = strings.ToUpper(testUser)
		}
		if _, err := dc.PurgeUserData(systemCtx, &deletion.PurgeUserDataRequest{UserId: id}); err != nil {
			t.Fatal(err)
		}
	}
	var remaining int
	if err := db.pool.QueryRow(ctx, "SELECT count(*) FROM service_data WHERE user_id=$1", testUser).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("uppercase RPC did not remove canonical user data: %d %v", remaining, err)
	}
	called = false
	_, err = client.GetProfile(userCtx, &user.GetProfileRequest{})
	if status.Code(err) != codes.Unauthenticated || called {
		t.Fatalf("handler called %v: %v", called, err)
	}
	assertReason(t, err, "TOKEN_REVOKED")
	streamCalled = false
	deletedStream, err := ac.WatchAnswer(userCtx, &assistant.WatchAnswerRequest{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = deletedStream.Recv()
	if status.Code(err) != codes.Unauthenticated || streamCalled {
		t.Fatalf("deleted stream %v %v", streamCalled, err)
	}
	assertReason(t, err, "TOKEN_REVOKED")
}

// Test fakes replace only identity's declared verification dependencies; its real
// published ACL and trusted context implementation run in the server chain.
type serviceChecker struct{}

func (serviceChecker) VerifyService(_ context.Context, token string) (string, error) {
	if token == "bank-bff" || token == "user" {
		return token, nil
	}
	return "", errors.New("invalid service token")
}

type userChecker struct{}

func (userChecker) VerifyUser(_ context.Context, token string) (string, error) {
	if token == "valid-user" {
		return testUser, nil
	}
	return "", errors.New("invalid user token")
}

type answerServer struct {
	assistant.UnimplementedAssistantServiceServer
	called *bool
}

func (s answerServer) WatchAnswer(_ *assistant.WatchAnswerRequest, stream grpc.ServerStreamingServer[assistant.WatchAnswerResponse]) error {
	*s.called = true
	return stream.Send(&assistant.WatchAnswerResponse{})
}

func waitForAdvisory(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid uint32) {
	t.Helper()
	deadline, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		var blocked bool
		if err := pool.QueryRow(deadline, "SELECT EXISTS (SELECT 1 FROM pg_locks WHERE pid=$1 AND locktype='advisory' AND NOT granted)", pid).Scan(&blocked); err != nil {
			t.Fatalf("second transaction never waited on the user lock: %v", err)
		}
		if blocked {
			return
		}
		runtime.Gosched()
	}
}

func TestConcurrentPurgeCanonicalUUID(t *testing.T) {
	ctx, db, s := setup(t)
	if err := db.Within(ctx, func(ctx context.Context) error { return writeData(ctx, db, s, testUser) }); err != nil {
		t.Fatal(err)
	}
	first, err := db.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Rollback(ctx) }()
	firstCtx := context.WithValue(ctx, txKey{}, first)
	var callbacks atomic.Int32
	remove := func(ctx context.Context, id string) error {
		callbacks.Add(1)
		if id != testUser {
			t.Errorf("callback ID %q is not canonical", id)
		}
		return deleteData(db, id)(ctx)
	}
	if err := s.Purge(firstCtx, strings.ToUpper(testUser), remove); err != nil {
		t.Fatal(err)
	}
	ready := make(chan uint32)
	done := make(chan error, 1)
	go func() {
		done <- db.Within(ctx, func(ctx context.Context) error {
			tx, _ := db.Tx(ctx)
			ready <- tx.Conn().PgConn().PID()
			return s.Purge(ctx, testUser, remove)
		})
	}()
	waitForAdvisory(t, ctx, db.pool, <-ready)
	if err := first.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := callbacks.Load(); got != 1 {
		t.Fatalf("callbacks %d want 1", got)
	}
	var markers, remaining int
	if err := db.pool.QueryRow(ctx, "SELECT count(*) FROM tombstone_users").Scan(&markers); err != nil || markers != 1 {
		t.Fatalf("markers %d %v", markers, err)
	}
	if err := db.pool.QueryRow(ctx, "SELECT count(*) FROM service_data").Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("data %d %v", remaining, err)
	}
	for _, id := range []string{testUser, strings.ToUpper(testUser)} {
		deleted, err := s.Deleted(ctx, id)
		if err != nil || !deleted {
			t.Fatalf("Deleted(%q) %v %v", id, deleted, err)
		}
		if err := db.Within(ctx, func(ctx context.Context) error {
			alive, err := s.Guard(ctx, id)
			if err == nil && alive {
				t.Errorf("Guard(%q) admitted deleted user", id)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestGuardCancellationWhileWaiting(t *testing.T) {
	ctx, db, s := setup(t)
	holder, err := db.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := s.Guard(context.WithValue(ctx, txKey{}, holder), testUser); err != nil {
		t.Fatal(err)
	}
	waiter, err := db.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = waiter.Rollback(context.Background()) }()
	waiting, cancel := context.WithCancel(context.WithValue(ctx, txKey{}, waiter))
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := s.Guard(waiting, testUser); done <- err }()
	waitForAdvisory(t, ctx, db.pool, waiter.Conn().PgConn().PID())
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled lock wait: %v", err)
	}
}
