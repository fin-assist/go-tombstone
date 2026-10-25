package tombstone_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	deletion "github.com/fin-assist/contracts/gen/go/finassist/userdeletion/v1"
	postgres "github.com/fin-assist/go-postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/fin-assist/go-tombstone"
	"github.com/fin-assist/go-tombstone/internal/pgtest"
)

const managerUser = "ae490c49-9bbf-410a-8bda-622befac3604"

type managerFixture struct {
	ctx     context.Context
	pool    *pgxpool.Pool
	manager *postgres.Manager
	store   *tombstone.Store
}

func newManagerFixture(t *testing.T, connections int32) *managerFixture {
	t.Helper()
	// The first DSN call may start a PostgreSQL container; only the scenario gets the deadline.
	dsn := pgtest.DSN(t)
	pgtest.Apply(t, dsn, tombstone.MigrationsTable, tombstone.Migrations)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.MaxConns = connections
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `
		CREATE TABLE service_records (user_id uuid PRIMARY KEY, value text NOT NULL);
		CREATE TABLE receipt_parents (id uuid PRIMARY KEY);
		CREATE TABLE purge_receipts (
			user_id uuid PRIMARY KEY,
			CONSTRAINT fin244_receipt_parent_fk FOREIGN KEY (user_id) REFERENCES receipt_parents(id)
			DEFERRABLE INITIALLY DEFERRED
		)`)
	if err != nil {
		t.Fatal(err)
	}
	manager := postgres.New(pool)
	store, err := tombstone.New(manager, pool, func() time.Time { return time.Unix(244, 0) })
	if err != nil {
		t.Fatal(err)
	}
	return &managerFixture{ctx: ctx, pool: pool, manager: manager, store: store}
}

func (f *managerFixture) write(ctx context.Context) error {
	alive, err := f.store.Guard(ctx, managerUser)
	if err != nil {
		return err
	}
	if !alive {
		return errors.New("service: deleted user")
	}
	tx, ok := f.manager.Tx(ctx)
	if !ok {
		return tombstone.ErrTransactionRequired
	}
	_, err = tx.Exec(ctx, "INSERT INTO service_records VALUES ($1, 'private business data')", managerUser)
	return err
}

func (f *managerFixture) remove(ctx context.Context, id string) error {
	tx, ok := f.manager.Tx(ctx)
	if !ok {
		return tombstone.ErrTransactionRequired
	}
	_, err := tx.Exec(ctx, "DELETE FROM service_records WHERE user_id=$1", id)
	return err
}

func (f *managerFixture) assertState(t *testing.T, records, tombstones, receipts int) {
	t.Helper()
	var gotRecords, gotTombstones, gotReceipts int
	err := f.pool.QueryRow(f.ctx, `SELECT
		(SELECT count(*) FROM service_records),
		(SELECT count(*) FROM tombstone_users),
		(SELECT count(*) FROM purge_receipts)`).Scan(&gotRecords, &gotTombstones, &gotReceipts)
	if err != nil {
		t.Fatal(err)
	}
	if gotRecords != records || gotTombstones != tombstones || gotReceipts != receipts {
		t.Fatalf("records/tombstones/receipts = %d/%d/%d, want %d/%d/%d", gotRecords, gotTombstones, gotReceipts, records, tombstones, receipts)
	}
}

func TestAPIManagerGuardWriteCommitAndRollback(t *testing.T) {
	f := newManagerFixture(t, 1)
	refused := errors.New("service refuses write")
	err := f.manager.Run(f.ctx, func(ctx context.Context) error {
		if err := f.write(ctx); err != nil {
			return err
		}
		return refused
	})
	if !errors.Is(err, refused) {
		t.Fatalf("write rollback = %v", err)
	}
	f.assertState(t, 0, 0, 0)
	if err := f.manager.Run(f.ctx, f.write); err != nil {
		t.Fatal(err)
	}
	f.assertState(t, 1, 0, 0)
	if err := f.manager.Run(f.ctx, func(ctx context.Context) error {
		return f.store.Purge(ctx, managerUser, f.remove)
	}); err != nil {
		t.Fatal(err)
	}
	f.assertState(t, 0, 1, 0)
	if deleted, err := f.store.Deleted(f.ctx, managerUser); err != nil || !deleted {
		t.Fatalf("primary Deleted = %v, %v", deleted, err)
	}
	if err := f.manager.Run(f.ctx, f.write); err == nil {
		t.Fatal("Guard allowed resurrection after committed Purge")
	}
	f.assertState(t, 0, 1, 0)
}

func TestAPIManagerPurgeCommitBoundary(t *testing.T) {
	for _, outcome := range []string{"commit", "callback failure", "deferred FK", "nested rollback"} {
		t.Run(outcome, func(t *testing.T) {
			f := newManagerFixture(t, 1)
			if err := f.manager.Run(f.ctx, f.write); err != nil {
				t.Fatal(err)
			}
			refused := errors.New("service refuses deletion")
			callbackCompleted := false
			err := f.manager.Run(f.ctx, func(ctx context.Context) error {
				outerTx, _ := f.manager.Tx(ctx)
				if err := f.manager.Run(ctx, func(nested context.Context) error {
					innerTx, _ := f.manager.Tx(nested)
					if innerTx != outerTx {
						return errors.New("nested Run replaced the service transaction")
					}
					return f.store.Purge(nested, managerUser, func(ctx context.Context, id string) error {
						if err := f.remove(ctx, id); err != nil {
							return err
						}
						if outcome == "callback failure" {
							return refused
						}
						// The immediate statement succeeds; the actual outer COMMIT checks the FK.
						_, err := innerTx.Exec(ctx, "INSERT INTO purge_receipts VALUES ($1)", id)
						return err
					})
				}); err != nil {
					return err
				}
				callbackCompleted = true
				if outcome == "nested rollback" {
					return refused
				}
				if outcome == "commit" {
					// This repair after nested Run proves that it did not attempt an early COMMIT.
					_, err := outerTx.Exec(ctx, "INSERT INTO receipt_parents VALUES ($1)", managerUser)
					return err
				}
				return nil
			})
			switch outcome {
			case "commit":
				if err != nil || !callbackCompleted {
					t.Fatalf("commit = %v, callback completed = %v", err, callbackCompleted)
				}
				f.assertState(t, 0, 1, 1)
			case "deferred FK":
				if !callbackCompleted || !postgres.IsViolation(err, "23503", "fin244_receipt_parent_fk") {
					t.Fatalf("outer COMMIT = %v, callback completed = %v", err, callbackCompleted)
				}
				f.assertState(t, 1, 0, 0)
			default:
				if !errors.Is(err, refused) {
					t.Fatalf("rollback = %v", err)
				}
				f.assertState(t, 1, 0, 0)
			}
		})
	}
}

func TestAPIManagerNestedWithinPreservesOuterIsolation(t *testing.T) {
	for _, isolation := range []pgx.TxIsoLevel{pgx.RepeatableRead, pgx.Serializable} {
		for _, operation := range []string{"Guard", "Purge"} {
			t.Run(string(isolation)+"/"+operation, func(t *testing.T) {
				f := newManagerFixture(t, 1)
				if err := f.manager.Run(f.ctx, f.write); err != nil {
					t.Fatal(err)
				}
				nestedCalled, effectCalled := false, false
				err := f.manager.RunWith(f.ctx, pgx.TxOptions{IsoLevel: isolation}, func(ctx context.Context) error {
					tx, _ := f.manager.Tx(ctx)
					var before, after string
					if err := tx.QueryRow(ctx, "SHOW transaction_isolation").Scan(&before); err != nil {
						return err
					}
					err := (managerWithin{f.manager}).Within(ctx, func(nested context.Context) error {
						nestedCalled = true
						if operation == "Purge" {
							return f.store.Purge(nested, managerUser, func(ctx context.Context, id string) error {
								effectCalled = true
								return f.remove(ctx, id)
							})
						}
						alive, err := f.store.Guard(nested, managerUser)
						if err != nil {
							return err
						}
						if alive {
							effectCalled = true
							return f.remove(nested, managerUser)
						}
						return nil
					})
					if scanErr := tx.QueryRow(ctx, "SHOW transaction_isolation").Scan(&after); scanErr != nil {
						return scanErr
					}
					if before != string(isolation) || after != before {
						return fmt.Errorf("outer isolation changed: before=%q, after=%q, want=%q", before, after, isolation)
					}
					return err
				})
				if !errors.Is(err, tombstone.ErrIsolation) || !nestedCalled || effectCalled {
					t.Fatalf("nested %s = %v, nested callback = %v, business effect = %v", operation, err, nestedCalled, effectCalled)
				}
				f.assertState(t, 1, 0, 0)
			})
		}
	}
}

func TestAPIManagerForeignContextHasNoEffects(t *testing.T) {
	f := newManagerFixture(t, 1)
	foreign := postgres.New(f.pool)
	otherStore, err := tombstone.New(foreign, f.pool, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	err = f.manager.Run(f.ctx, func(ctx context.Context) error {
		if tx, ok := foreign.Tx(ctx); ok || tx != nil {
			return errors.New("foreign manager exposed an owned transaction")
		}
		if _, err := otherStore.Guard(ctx, managerUser); !errors.Is(err, tombstone.ErrTransactionRequired) {
			return fmt.Errorf("foreign Guard = %w", err)
		}
		called := false
		if err := otherStore.Purge(ctx, managerUser, func(context.Context, string) error {
			called = true
			return nil
		}); !errors.Is(err, tombstone.ErrTransactionRequired) || called {
			return fmt.Errorf("foreign Purge = %w, callback = %v", err, called)
		}
		if err := foreign.Run(ctx, func(context.Context) error {
			called = true
			return nil
		}); !errors.Is(err, postgres.ErrForeignTransaction) || called {
			return fmt.Errorf("foreign Run = %w, callback = %v", err, called)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	f.assertState(t, 0, 0, 0)
}

func TestAPIManagerDeletionServerSanitizesCommitFailure(t *testing.T) {
	f := newManagerFixture(t, 1)
	if err := f.manager.Run(f.ctx, f.write); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	var failCommit atomic.Bool
	failCommit.Store(true)
	handler, err := tombstone.NewDeletionServer(managerWithin{f.manager}, f.store, func(ctx context.Context, id string) error {
		calls.Add(1)
		if err := f.remove(ctx, id); err != nil {
			return err
		}
		if failCommit.Load() {
			tx, _ := f.manager.Tx(ctx)
			_, err := tx.Exec(ctx, "INSERT INTO purge_receipts VALUES ($1)", id)
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	deletion.RegisterUserDeletionServiceServer(server, handler)
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
				t.Errorf("gRPC server stopped: %v", err)
			}
		case <-f.ctx.Done():
			t.Error("gRPC server did not stop")
		}
	})
	conn, err := grpc.NewClient("passthrough:///fin244", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return listener.DialContext(ctx)
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := deletion.NewUserDeletionServiceClient(conn)
	request := &deletion.PurgeUserDataRequest{UserId: managerUser}
	if _, err := client.PurgeUserData(f.ctx, request); status.Code(err) != codes.Internal || status.Convert(err).Message() != "internal error" || len(status.Convert(err).Details()) != 0 {
		t.Fatalf("COMMIT failure response must be a bare INTERNAL: %v", err)
	}
	f.assertState(t, 1, 0, 0)
	failCommit.Store(false)
	for range 2 {
		if _, err := client.PurgeUserData(f.ctx, request); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("remove calls = %d, want failed attempt + one committed deletion", calls.Load())
	}
	f.assertState(t, 0, 1, 0)
	if _, err := client.PurgeUserData(f.ctx, &deletion.PurgeUserDataRequest{UserId: strings.ToUpper(managerUser)}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatal("canonical UUID replay ran removal again")
	}
}
