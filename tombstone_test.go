package tombstone

import (
	"context"
	"errors"
	"testing"
	"time"

	deletion "github.com/fin-assist/contracts/gen/go/finassist/userdeletion/v1"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const testUser = "019cf000-0000-7000-8000-000000000001"
const otherUser = "019cf000-0000-7000-8000-000000000002"

type fakeChecker struct {
	deleted bool
	err     error
	calls   int
}

func (f *fakeChecker) Deleted(context.Context, string) (bool, error) {
	f.calls++
	return f.deleted, f.err
}

type fakeReader struct {
	err     error
	deleted bool
}

func (f fakeReader) QueryRow(context.Context, string, ...any) pgx.Row {
	return fakeRow(f)
}

type fakeRow struct {
	err     error
	deleted bool
}

func (f fakeRow) Scan(dst ...any) error {
	if f.err != nil {
		return f.err
	}
	*(dst[0].(*bool)) = f.deleted
	return nil
}

type missingTx struct{}

func (missingTx) Tx(context.Context) (pgx.Tx, bool) { return nil, false }

func TestStoreValidation(t *testing.T) {
	ctx := context.Background()
	now := func() time.Time { return time.Unix(100, 0) }
	for _, tc := range []struct {
		tx    Transactions
		r     Reader
		clock func() time.Time
	}{{nil, fakeReader{}, now}, {missingTx{}, nil, now}, {missingTx{}, fakeReader{}, nil}} {
		if _, err := New(tc.tx, tc.r, tc.clock); err == nil {
			t.Fatal("missing dependencies accepted")
		}
	}
	s, err := New(missingTx{}, fakeReader{}, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "not-a-uuid", "019CF000-0000-7000-8000-000000000001", string(make([]byte, 129))} {
		if _, err := s.Deleted(ctx, id); !errors.Is(err, ErrUserID) {
			t.Fatal(err)
		}
		if _, err := s.Guard(ctx, id); !errors.Is(err, ErrUserID) {
			t.Fatal(err)
		}
	}
	if _, err := s.Guard(ctx, testUser); !errors.Is(err, ErrTransactionRequired) {
		t.Fatal(err)
	}
	if err := s.Purge(ctx, testUser, nil); err == nil {
		t.Fatal("nil remove")
	}
	if err := s.Purge(ctx, testUser, func(context.Context) error { return nil }); !errors.Is(err, ErrTransactionRequired) {
		t.Fatal(err)
	}
	for _, tc := range []fakeReader{{}, {deleted: true}, {err: errors.New("secret SQL")}} {
		s.primary = tc
		got, err := s.Deleted(ctx, testUser)
		if tc.err != nil {
			if !errors.Is(err, ErrStorage) {
				t.Fatal(err)
			}
		} else if err != nil || got != tc.deleted {
			t.Fatalf("%v %v", got, err)
		}
	}
}

func TestInterceptors(t *testing.T) {
	for _, tc := range []struct{ system, deleted, failed bool }{{}, {deleted: true}, {failed: true}, {system: true, deleted: true}} {
		t.Run("admission", func(t *testing.T) {
			check := &fakeChecker{deleted: tc.deleted}
			if tc.failed {
				check.err = errors.New("secret")
			}
			reg := prometheus.NewRegistry()
			metric, err := NewRevokedCounter(reg)
			if err != nil {
				t.Fatal(err)
			}
			getUser := func(context.Context) string {
				if tc.system {
					return ""
				}
				return testUser
			}
			unary, stream, err := Interceptors(check, getUser, metric)
			if err != nil {
				t.Fatal(err)
			}
			called := false
			_, err = unary(context.Background(), nil, &grpc.UnaryServerInfo{}, func(context.Context, any) (any, error) { called = true; return "ok", nil })
			want := codes.OK
			if !tc.system && tc.deleted {
				want = codes.Unauthenticated
			}
			if tc.failed {
				want = codes.Internal
			}
			if status.Code(err) != want || called != (want == codes.OK) {
				t.Fatalf("called %v, err %v", called, err)
			}
			if want == codes.Unauthenticated {
				assertReason(t, err, "TOKEN_REVOKED")
			}
			called = false
			err = stream(nil, fakeStream{ctx: context.Background()}, &grpc.StreamServerInfo{}, func(any, grpc.ServerStream) error { called = true; return nil })
			if status.Code(err) != want || called != (want == codes.OK) {
				t.Fatalf("stream %v %v", called, err)
			}
			wantCount := float64(0)
			if want == codes.Unauthenticated {
				wantCount = 2
			}
			if testutil.ToFloat64(metric) != wantCount {
				t.Fatal("counter")
			}
		})
	}
	if _, _, err := Interceptors(nil, func(context.Context) string { return "" }, nil); err == nil {
		t.Fatal("nil checker")
	}
	if _, _, err := Interceptors(&fakeChecker{}, nil, nil); err == nil {
		t.Fatal("nil identity")
	}
	if _, err := NewRevokedCounter(nil); err == nil {
		t.Fatal("nil registry")
	}
	reg := prometheus.NewRegistry()
	if _, err := NewRevokedCounter(reg); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRevokedCounter(reg); err == nil {
		t.Fatal("duplicate registration")
	}
}

type fakeStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (f fakeStream) Context() context.Context { return f.ctx }
func assertReason(t *testing.T, err error, want string) {
	t.Helper()
	for _, d := range status.Convert(err).Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			if info.Reason != want || info.Domain != "finassist" {
				t.Fatalf("%v", info)
			}
			return
		}
	}
	t.Fatal("no reason")
}

type fakeTransactor struct{ err error }

func (f fakeTransactor) Within(ctx context.Context, fn func(context.Context) error) error {
	if f.err != nil {
		return f.err
	}
	return fn(ctx)
}

type fakePurger struct {
	err   error
	calls int
}

func (f *fakePurger) Purge(ctx context.Context, _ string, remove func(context.Context) error) error {
	f.calls++
	if f.err != nil {
		return f.err
	}
	return remove(ctx)
}
func TestDeletionServer(t *testing.T) {
	remove := func(context.Context, string) error { return nil }
	p := &fakePurger{}
	if _, err := NewDeletionServer(nil, p, remove); err == nil {
		t.Fatal("nil tx")
	}
	if _, err := NewDeletionServer(fakeTransactor{}, nil, remove); err == nil {
		t.Fatal("nil store")
	}
	if _, err := NewDeletionServer(fakeTransactor{}, p, nil); err == nil {
		t.Fatal("nil remove")
	}
	for _, mode := range []string{"ok", "tx", "purge", "remove", "nil", "empty"} {
		t.Run(mode, func(t *testing.T) {
			tx := fakeTransactor{}
			p := &fakePurger{}
			removed := false
			if mode == "tx" {
				tx.err = errors.New("secret")
			}
			if mode == "purge" {
				p.err = errors.New("secret")
			}
			s, err := NewDeletionServer(tx, p, func(context.Context, string) error {
				removed = true
				if mode == "remove" {
					return errors.New("secret")
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			req := &deletion.PurgeUserDataRequest{UserId: testUser}
			if mode == "nil" {
				req = nil
			}
			if mode == "empty" {
				req.UserId = ""
			}
			_, err = s.PurgeUserData(context.Background(), req)
			if mode == "ok" {
				if err != nil || !removed {
					t.Fatalf("%v %v", err, removed)
				}
			} else if status.Code(err) != codes.Internal || status.Convert(err).Message() != "internal error" {
				t.Fatal(err)
			}
		})
	}
}

type fakeTxProvider struct{ tx pgx.Tx }

func (f fakeTxProvider) Tx(context.Context) (pgx.Tx, bool) { return f.tx, true }

type scriptedRow struct {
	value any
	err   error
}

func (r scriptedRow) Scan(dst ...any) error {
	if r.err != nil {
		return r.err
	}
	switch v := r.value.(type) {
	case string:
		*(dst[0].(*string)) = v
	case bool:
		*(dst[0].(*bool)) = v
	}
	return nil
}

type scriptedTx struct {
	pgx.Tx
	rows       []scriptedRow
	execErrors []error
	execCalls  int
}

func (f *scriptedTx) QueryRow(context.Context, string, ...any) pgx.Row {
	r := f.rows[0]
	f.rows = f.rows[1:]
	return r
}
func (f *scriptedTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	i := f.execCalls
	f.execCalls++
	if i < len(f.execErrors) {
		return pgconn.CommandTag{}, f.execErrors[i]
	}
	return pgconn.CommandTag{}, nil
}
func TestTransactionFailures(t *testing.T) {
	boom := errors.New("secret SQL")
	for _, tc := range []struct {
		name    string
		rows    []scriptedRow
		exec    []error
		want    error
		removed bool
	}{
		{name: "isolation query", rows: []scriptedRow{{err: boom}}, want: ErrStorage},
		{name: "isolation rejected", rows: []scriptedRow{{value: "repeatable read"}}, want: ErrIsolation},
		{name: "lock", rows: []scriptedRow{{value: "read committed"}}, exec: []error{boom}, want: ErrStorage},
		{name: "read", rows: []scriptedRow{{value: "read committed"}, {err: boom}}, want: ErrStorage},
		{name: "already deleted", rows: []scriptedRow{{value: "read committed"}, {value: true}}},
		{name: "mark", rows: []scriptedRow{{value: "read committed"}, {value: false}}, exec: []error{nil, boom}, want: ErrStorage, removed: true},
		{name: "success", rows: []scriptedRow{{value: "read committed"}, {value: false}}, removed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx := &scriptedTx{rows: tc.rows, execErrors: tc.exec}
			s, err := New(fakeTxProvider{tx}, fakeReader{}, func() time.Time { return time.Unix(100, 0) })
			if err != nil {
				t.Fatal(err)
			}
			removed := false
			err = s.Purge(context.Background(), testUser, func(context.Context) error { removed = true; return nil })
			if !errors.Is(err, tc.want) || removed != tc.removed {
				t.Fatalf("error %v want %v, removed %v", err, tc.want, removed)
			}
		})
	}
}
