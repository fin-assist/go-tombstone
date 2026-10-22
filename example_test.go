package tombstone_test

import (
	"context"
	"time"

	"github.com/fin-assist/contracts/gen/go/finerr"
	"github.com/fin-assist/go-identity"
	"google.golang.org/grpc"

	"github.com/fin-assist/go-tombstone"
)

func ExampleNew() {
	var transactions tombstone.Transactions
	var primary tombstone.Reader
	store, err := tombstone.New(transactions, primary, time.Now)
	if err != nil {
		return
	}
	_ = store // Inject real service dependencies; the service runs library migrations first.
}

func ExampleStore_Guard() {
	var store *tombstone.Store
	write := func(ctx context.Context, userID string) error {
		// ctx must already carry the service's READ COMMITTED transaction.
		alive, err := store.Guard(ctx, userID)
		if err != nil {
			return err
		}
		if !alive {
			return finerr.TokenRevoked()
		}
		return nil // Write the effect using the same transaction, then let the service commit.
	}
	_ = write
}

func ExampleStore_Purge() {
	var store *tombstone.Store
	purge := func(ctx context.Context, userID string) error {
		return store.Purge(ctx, userID, func(ctx context.Context) error {
			return nil // Delete all user-owned service rows using the transaction in ctx.
		})
	}
	_ = purge // The service rolls back the whole transaction on any error.
}

func ExampleInterceptors() {
	var store *tombstone.Store
	var auth *identity.Server
	unary, stream, err := tombstone.Interceptors(store, func(ctx context.Context) string {
		userID, _ := identity.UserID(ctx)
		return userID
	}, nil)
	if err != nil {
		return
	}
	server := grpc.NewServer(grpc.ChainUnaryInterceptor(auth.Unary(), unary), grpc.ChainStreamInterceptor(auth.Stream(), stream))
	_ = server
}

func ExampleNewDeletionServer() {
	var transactions tombstone.Transactor
	var store *tombstone.Store
	server, err := tombstone.NewDeletionServer(transactions, store, func(ctx context.Context, userID string) error {
		return nil // Delete the service's user-owned rows using transactions.Tx(ctx).
	})
	if err != nil {
		return
	}
	_ = server // Register the generated service behind the identity ACL allowing only user.
}
