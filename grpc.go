package tombstone

import (
	"context"
	"errors"

	deletion "github.com/fin-assist/contracts/gen/go/finassist/userdeletion/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Checker checks tombstones on the primary without negative caching.
type Checker interface {
	Deleted(context.Context, string) (bool, error)
}

// UserFromContext returns the verified user subject, or empty for an authenticated
// system call. It MUST read only trusted identity context, never request metadata.
type UserFromContext func(context.Context) string

// Interceptors checks every user call, including streams at admission. Install after
// identity and service ACL interceptors. Recheck Guard in every writing transaction.
// The counter has no labels and counts only TOKEN_REVOKED responses. It may be nil.
// Dependencies must be non-nil; this constructor returns an error otherwise.
func Interceptors(check Checker, user UserFromContext, revoked prometheus.Counter) (grpc.UnaryServerInterceptor, grpc.StreamServerInterceptor, error) {
	if check == nil || user == nil {
		return nil, nil, errors.New("tombstone: identity and checker required")
	}
	admit := func(ctx context.Context) error {
		id := user(ctx)
		if id == "" {
			return nil
		}
		deleted, err := check.Deleted(ctx, id)
		if err != nil {
			return internalError()
		}
		if deleted {
			if revoked != nil {
				revoked.Inc()
			}
			return finerr.TokenRevoked()
		}
		return nil
	}
	unary := func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := admit(ctx); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
	stream := func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := admit(ss.Context()); err != nil {
			return err
		}
		return handler(srv, ss)
	}
	return unary, stream, nil
}

// NewRevokedCounter registers a bounded, label-free metric on the supplied registry.
// Registration errors are returned; no global registry is used.
func NewRevokedCounter(reg prometheus.Registerer) (prometheus.Counter, error) {
	if reg == nil {
		return nil, errors.New("tombstone: registry required")
	}
	counter := prometheus.NewCounter(prometheus.CounterOpts{Name: "finassist_tombstone_revoked_total", Help: "User calls rejected because their user was deleted."})
	if err := reg.Register(counter); err != nil {
		return nil, err
	}
	return counter, nil
}

// Transactor executes fn in a service-owned transaction and rolls back on any error.
// It must commit before returning nil. Nested transactions are the service's concern.
type Transactor interface {
	Within(context.Context, func(context.Context) error) error
}

// Purger is implemented by Store. remove must participate in the current transaction.
type Purger interface {
	Purge(context.Context, string, func(context.Context) error) error
}

// DeletionServer implements the generated PurgeUserData RPC. Install identity/ACL
// allowing only the user service, and protovalidate before this handler. It is safe
// for concurrent use when its dependencies are. It never authenticates metadata itself.
type DeletionServer struct {
	deletion.UnimplementedUserDeletionServiceServer
	transactions Transactor
	store        Purger
	remove       func(context.Context, string) error
}

// NewDeletionServer wires the service's transaction runner and deletion callback.
// Missing dependencies return an error. The callback must delete every user-owned table.
func NewDeletionServer(tx Transactor, store Purger, remove func(context.Context, string) error) (*DeletionServer, error) {
	if tx == nil || store == nil || remove == nil {
		return nil, errors.New("tombstone: deletion dependencies required")
	}
	return &DeletionServer{transactions: tx, store: store, remove: remove}, nil
}

// PurgeUserData atomically deletes service data and writes the tombstone. Replays
// return OK. All internal failures are sanitized; the service logs errors separately.
func (s *DeletionServer) PurgeUserData(ctx context.Context, req *deletion.PurgeUserDataRequest) (*deletion.PurgeUserDataResponse, error) {
	if req == nil || validID(req.GetUserId()) != nil {
		return nil, internalError()
	}
	err := s.transactions.Within(ctx, func(ctx context.Context) error {
		return s.store.Purge(ctx, req.GetUserId(), func(ctx context.Context) error { return s.remove(ctx, req.GetUserId()) })
	})
	if err != nil {
		return nil, internalError()
	}
	return &deletion.PurgeUserDataResponse{}, nil
}

func internalError() error {
	//nolint:forbidigo // Internal refusal deliberately carries no client-facing ErrorInfo or storage details.
	return status.Error(codes.Internal, "internal error")
}
