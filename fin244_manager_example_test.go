package tombstone_test

import (
	"context"
	"time"

	postgres "github.com/fin-assist/go-postgres"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fin-assist/go-tombstone"
)

func ExampleNew_goPostgres() {
	// The service applies tombstone.Migrations and its own migrations before wiring.
	wire := func(primary *pgxpool.Pool) (*tombstone.DeletionServer, error) {
		manager := postgres.New(primary) // One instance shared by service adapters.
		store, err := tombstone.New(manager, primary, time.Now)
		if err != nil {
			return nil, err
		}
		// managerWithin delegates Within to RunWith(READ COMMITTED), without owning a second transaction.
		return tombstone.NewDeletionServer(managerWithin{manager}, store, func(ctx context.Context, id string) error {
			tx, ok := manager.Tx(ctx)
			if !ok {
				return tombstone.ErrTransactionRequired
			}
			_, err := tx.Exec(ctx, "DELETE FROM service_records WHERE user_id=$1", id)
			return err
		})
	}
	_ = wire // Register the generated deletion service behind the service identity/ACL chain.
}
