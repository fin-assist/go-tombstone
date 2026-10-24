package tombstone_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	deletion "github.com/fin-assist/contracts/gen/go/finassist/userdeletion/v1"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	tombstone "github.com/fin-assist/go-tombstone"
	"github.com/fin-assist/go-tombstone/internal/pgtest"
)

type transactionKey struct{}
type consumerDB struct{ pool *pgxpool.Pool }

func (d consumerDB) Tx(ctx context.Context) (pgx.Tx, bool) {
	tx, ok := ctx.Value(transactionKey{}).(pgx.Tx)
	return tx, ok
}
func (d consumerDB) Within(ctx context.Context, fn func(context.Context) error) error {
	tx, err := d.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err = fn(context.WithValue(ctx, transactionKey{}, tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func publicStore(t *testing.T) (context.Context, consumerDB, *tombstone.Store) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	dsn := pgtest.DSN(t)
	pgtest.Apply(t, dsn, tombstone.MigrationsTable, tombstone.Migrations)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err = pool.Exec(ctx, "CREATE TABLE consumer_data(user_id uuid PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	db := consumerDB{pool}
	store, err := tombstone.New(db, pool, func() time.Time { return time.Unix(123, 0) })
	if err != nil {
		t.Fatal(err)
	}
	return ctx, db, store
}

const owner = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
const foreign = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"

func TestAPIConsumerAtomicDeletionAndAdmission(t *testing.T) {
	ctx, db, store := publicStore(t)
	if _, err := db.pool.Exec(ctx, "INSERT INTO consumer_data VALUES($1),($2)", owner, foreign); err != nil {
		t.Fatal(err)
	}
	remove := func(ctx context.Context, id string) error {
		tx, ok := db.Tx(ctx)
		if !ok {
			t.Fatal("callback lost caller transaction")
		}
		if id != owner {
			t.Fatalf("noncanonical callback %q", id)
		}
		_, err := tx.Exec(ctx, "DELETE FROM consumer_data WHERE user_id=$1", id)
		return err
	}
	aborted := errors.New("consumer effect failed")
	if err := db.Within(ctx, func(ctx context.Context) error {
		if err := store.Purge(ctx, strings.ToUpper(owner), remove); err != nil {
			return err
		}
		return aborted
	}); !errors.Is(err, aborted) {
		t.Fatal(err)
	}
	if deleted, err := store.Deleted(ctx, owner); err != nil || deleted {
		t.Fatalf("rolled-back marker %v %v", deleted, err)
	}
	var count int
	if err := db.pool.QueryRow(ctx, "SELECT count(*) FROM consumer_data").Scan(&count); err != nil || count != 2 {
		t.Fatalf("rollback data=%d %v", count, err)
	}
	if err := db.Within(ctx, func(ctx context.Context) error { return store.Purge(ctx, owner, remove) }); err != nil {
		t.Fatal(err)
	}
	if err := db.Within(ctx, func(ctx context.Context) error {
		return store.Purge(ctx, owner, func(context.Context, string) error { t.Error("replay removed twice"); return nil })
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Within(ctx, func(ctx context.Context) error {
		alive, err := store.Guard(ctx, owner)
		if alive {
			t.Error("late event may resurrect user")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if deleted, err := store.Deleted(ctx, foreign); err != nil || deleted {
		t.Fatalf("foreign user deleted %v %v", deleted, err)
	}
	if err := db.pool.QueryRow(ctx, "SELECT count(*) FROM consumer_data WHERE user_id=$1", foreign).Scan(&count); err != nil || count != 1 {
		t.Fatalf("foreign data=%d %v", count, err)
	}
	reg := prometheus.NewRegistry()
	counter, err := tombstone.NewRevokedCounter(reg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tombstone.NewRevokedCounter(reg); err == nil {
		t.Fatal("duplicate metric accepted")
	}
	unary, stream, err := tombstone.Interceptors(store, func(context.Context) string { return owner }, counter)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = unary(ctx, nil, &grpc.UnaryServerInfo{}, func(context.Context, any) (any, error) { t.Error("deleted user admitted"); return nil, nil }); status.Code(err) != codes.Unauthenticated {
		t.Fatal(err)
	}
	if err = stream(nil, consumerStream{ctx: ctx}, &grpc.StreamServerInfo{}, func(any, grpc.ServerStream) error { t.Error("deleted stream admitted"); return nil }); status.Code(err) != codes.Unauthenticated {
		t.Fatal(err)
	}
	metrics, err := reg.Gather()
	if err != nil || len(metrics) != 1 || metrics[0].Metric[0].GetCounter().GetValue() != 2 {
		t.Fatalf("revoked metric %v %v", metrics, err)
	}
	handler, err := tombstone.NewDeletionServer(db, store, remove)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = handler.PurgeUserData(ctx, &deletion.PurgeUserDataRequest{UserId: owner}); err != nil {
		t.Fatal(err)
	}
}

type consumerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s consumerStream) Context() context.Context { return s.ctx }

func TestAPIConsumerErrorsFailClosed(t *testing.T) {
	ctx, db, store := publicStore(t)
	if _, err := store.Guard(ctx, owner); !errors.Is(err, tombstone.ErrTransactionRequired) {
		t.Fatal(err)
	}
	for _, id := range []string{"", "not-a-uuid"} {
		if _, err := store.Deleted(ctx, id); !errors.Is(err, tombstone.ErrUserID) {
			t.Fatal(err)
		}
		if _, err := store.Guard(ctx, id); !errors.Is(err, tombstone.ErrUserID) {
			t.Fatal(err)
		}
		if err := store.Purge(ctx, id, func(context.Context, string) error { t.Error("invalid removal"); return nil }); !errors.Is(err, tombstone.ErrUserID) {
			t.Fatal(err)
		}
	}
	if err := store.Purge(ctx, owner, nil); err == nil {
		t.Fatal("nil callback accepted")
	}
	for _, iso := range []pgx.TxIsoLevel{pgx.RepeatableRead, pgx.Serializable} {
		tx, err := db.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: iso})
		if err != nil {
			t.Fatal(err)
		}
		_, err = store.Guard(context.WithValue(ctx, transactionKey{}, tx), owner)
		_ = tx.Rollback(ctx)
		if !errors.Is(err, tombstone.ErrIsolation) {
			t.Fatal(err)
		}
	}
	failure := errors.New("consumer removal failed")
	if err := db.Within(ctx, func(ctx context.Context) error {
		return store.Purge(ctx, owner, func(context.Context, string) error { return failure })
	}); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if deleted, err := store.Deleted(ctx, owner); err != nil || deleted {
		t.Fatalf("failed deletion persisted %v %v", deleted, err)
	}
	for _, id := range []string{"", foreign} {
		unary, stream, err := tombstone.Interceptors(store, func(context.Context) string { return id }, nil)
		if err != nil {
			t.Fatal(err)
		}
		called := 0
		_, err = unary(ctx, nil, &grpc.UnaryServerInfo{}, func(context.Context, any) (any, error) { called++; return nil, nil })
		if err != nil {
			t.Fatal(err)
		}
		err = stream(nil, consumerStream{ctx: ctx}, &grpc.StreamServerInfo{}, func(any, grpc.ServerStream) error { called++; return nil })
		if err != nil || called != 2 {
			t.Fatalf("live/system calls=%d %v", called, err)
		}
	}
	handler, err := tombstone.NewDeletionServer(db, store, func(context.Context, string) error { return failure })
	if err != nil {
		t.Fatal(err)
	}
	for _, req := range []*deletion.PurgeUserDataRequest{nil, {UserId: "invalid"}, {UserId: owner}} {
		_, err = handler.PurgeUserData(ctx, req)
		s := status.Convert(err)
		if s.Code() != codes.Internal || s.Message() != "internal error" {
			t.Fatalf("unsafe status %v", err)
		}
	}
	if _, err = db.pool.Exec(ctx, "DROP TABLE tombstone_users"); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Deleted(ctx, owner); !errors.Is(err, tombstone.ErrStorage) {
		t.Fatal(err)
	}
	if err = db.Within(ctx, func(ctx context.Context) error { _, err := store.Guard(ctx, owner); return err }); !errors.Is(err, tombstone.ErrStorage) {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = store.Deleted(canceled, owner); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err = tombstone.New(nil, db.pool, time.Now); err == nil {
		t.Fatal("missing transactions accepted")
	}
	if _, err = tombstone.New(db, nil, time.Now); err == nil {
		t.Fatal("missing primary accepted")
	}
	if _, err = tombstone.New(db, db.pool, nil); err == nil {
		t.Fatal("missing clock accepted")
	}
	if _, _, err = tombstone.Interceptors(nil, func(context.Context) string { return owner }, nil); err == nil {
		t.Fatal("missing checker accepted")
	}
	if _, _, err = tombstone.Interceptors(store, nil, nil); err == nil {
		t.Fatal("missing identity accepted")
	}
	if _, err = tombstone.NewRevokedCounter(nil); err == nil {
		t.Fatal("missing registry accepted")
	}
	if _, err = tombstone.NewDeletionServer(nil, store, func(context.Context, string) error { return nil }); err == nil {
		t.Fatal("missing transactor accepted")
	}
}
