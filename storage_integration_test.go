package tombstone

import (
	"context"
	"net"
	"testing"

	user "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	deletion "github.com/fin-assist/contracts/gen/go/finassist/userdeletion/v1"
	"github.com/fin-assist/go-identity"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func TestIntegrationGRPCStorageFailuresRollbackAndRecover(t *testing.T) {
	ctx, db, store := setup(t)
	if err := db.Within(ctx, func(ctx context.Context) error { return writeData(ctx, db, store, testUser) }); err != nil {
		t.Fatal(err)
	}
	auth, err := identity.NewServer(identity.AccessTable{user.ProfileService_GetProfile_FullMethodName: {"bank-bff": identity.User}, deletion.UserDeletionService_PurgeUserData_FullMethodName: {"user": identity.System}}, serviceChecker{}, userChecker{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	unary, _, err := Interceptors(store, func(ctx context.Context) string { id, _ := identity.UserID(ctx); return id }, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewDeletionServer(db, store, func(ctx context.Context, id string) error { return deleteData(db, id)(ctx) })
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1024 * 1024)
	t.Cleanup(func() { _ = listener.Close() })
	server := grpc.NewServer(grpc.ChainUnaryInterceptor(auth.Unary(), unary))
	t.Cleanup(server.Stop)
	deletion.RegisterUserDeletionServiceServer(server, handler)
	called := false
	user.RegisterProfileServiceServer(server, profileServer{called: &called})
	go func() { _ = server.Serve(listener) }()
	conn, err := grpc.NewClient("passthrough:///storage-test", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	profiles := user.NewProfileServiceClient(conn)
	deletions := deletion.NewUserDeletionServiceClient(conn)
	userCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs("x-service-authorization", "Bearer bank-bff", "authorization", "Bearer valid-user"))
	systemCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs("x-service-authorization", "Bearer user"))
	if _, err = profiles.GetProfile(userCtx, &user.GetProfileRequest{}); err != nil || !called {
		t.Fatalf("initial admission called=%v %v", called, err)
	}
	assertSafeInternal := func(err error) {
		t.Helper()
		s := status.Convert(err)
		if s.Code() != codes.Internal || s.Message() != "internal error" || len(s.Details()) != 0 {
			t.Fatalf("unsafe storage failure: %v", err)
		}
	}
	exec := func(sql string) {
		t.Helper()
		if _, err := db.pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	exec("ALTER TABLE tombstone_users RENAME TO unavailable_tombstones")
	called = false
	_, err = profiles.GetProfile(userCtx, &user.GetProfileRequest{})
	assertSafeInternal(err)
	if called {
		t.Fatal("unavailable primary admitted user")
	}
	_, err = deletions.PurgeUserData(systemCtx, &deletion.PurgeUserDataRequest{UserId: testUser})
	assertSafeInternal(err)
	exec("ALTER TABLE unavailable_tombstones RENAME TO tombstone_users")
	exec("ALTER TABLE service_data RENAME TO unavailable_service_data")
	_, err = deletions.PurgeUserData(systemCtx, &deletion.PurgeUserDataRequest{UserId: testUser})
	assertSafeInternal(err)
	exec("ALTER TABLE unavailable_service_data RENAME TO service_data")
	deleted, err := store.Deleted(ctx, testUser)
	if err != nil || deleted {
		t.Fatalf("failed deletion marked user: %v %v", deleted, err)
	}
	var remaining int
	if err = db.pool.QueryRow(ctx, "SELECT count(*) FROM service_data WHERE user_id=$1", testUser).Scan(&remaining); err != nil || remaining != 1 {
		t.Fatalf("failed delete changed data=%d %v", remaining, err)
	}
	_, err = deletions.PurgeUserData(systemCtx, &deletion.PurgeUserDataRequest{UserId: "invalid"})
	assertSafeInternal(err)
	if _, err = deletions.PurgeUserData(systemCtx, &deletion.PurgeUserDataRequest{UserId: testUser}); err != nil {
		t.Fatalf("retry after repaired SQL: %v", err)
	}
	called = false
	_, err = profiles.GetProfile(userCtx, &user.GetProfileRequest{})
	if status.Code(err) != codes.Unauthenticated || called {
		t.Fatalf("deleted user called=%v %v", called, err)
	}
	assertReason(t, err, "TOKEN_REVOKED")
	if err = db.pool.QueryRow(ctx, "SELECT count(*) FROM service_data").Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("successful deletion left rows=%d %v", remaining, err)
	}
}
