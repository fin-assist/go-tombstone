package tombstone

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	// ErrTransactionRequired means the caller must start a service-owned transaction.
	ErrTransactionRequired = errors.New("tombstone: transaction required")
	// ErrIsolation means the transaction must use READ COMMITTED with a fresh snapshot per statement.
	ErrIsolation = errors.New("tombstone: READ COMMITTED required")
	// ErrUserID means the user identifier is not a UUID.
	ErrUserID = errors.New("tombstone: invalid user identifier")
	// ErrStorage hides database details; the service should return an internal failure.
	ErrStorage = errors.New("tombstone: storage unavailable")
)

// Transactions supplies the current service-owned transaction. Implementations must be concurrency safe.
type Transactions interface {
	Tx(context.Context) (pgx.Tx, bool)
}

// Reader reads on the primary, never a replica.
type Reader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// Store is safe for concurrent use when its dependencies are. It never owns a transaction.
type Store struct {
	tx      Transactions
	primary Reader
	now     func() time.Time
}

// New constructs a store. All dependencies are required; now is the service clock.
func New(tx Transactions, primary Reader, now func() time.Time) (*Store, error) {
	if tx == nil || primary == nil || now == nil {
		return nil, errors.New("tombstone: dependencies required")
	}
	return &Store{tx: tx, primary: primary, now: now}, nil
}

func canonicalID(id string) (string, error) {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return "", ErrUserID
	}
	return parsed.String(), nil
}

// Deleted checks the primary without caching. Errors are ErrUserID, ErrStorage, or the context cancellation error.
// This admission check is not a substitute for Guard inside every writing transaction.
func (s *Store) Deleted(ctx context.Context, id string) (bool, error) {
	id, err := canonicalID(id)
	if err != nil {
		return false, err
	}
	var deleted bool
	if err := s.primary.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM tombstone_users WHERE user_id = $1)", id).Scan(&deleted); err != nil {
		return false, storageError(ctx)
	}
	return deleted, nil
}

// Guard serializes writes and deletion for id until the service commits or rolls back.
// Call before all user-data writes and hold the transaction until their completion.
// A false result means the user is deleted: system receivers return OK without effects;
// user requests return TOKEN_REVOKED. Errors: ErrUserID, ErrTransactionRequired,
// ErrIsolation, ErrStorage, or the context cancellation error. Only READ COMMITTED
// transactions are accepted. IDs are normalized before both locking and lookup.
func (s *Store) Guard(ctx context.Context, id string) (bool, error) {
	id, err := canonicalID(id)
	if err != nil {
		return false, err
	}
	tx, err := s.lock(ctx, id)
	if err != nil {
		return false, err
	}
	var deleted bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM tombstone_users WHERE user_id = $1)", id).Scan(&deleted); err != nil {
		return false, storageError(ctx)
	}
	return !deleted, nil
}

func (s *Store) lock(ctx context.Context, id string) (pgx.Tx, error) {
	tx, ok := s.tx.Tx(ctx)
	if !ok || tx == nil {
		return nil, ErrTransactionRequired
	}
	var isolation string
	if err := tx.QueryRow(ctx, "SHOW transaction_isolation").Scan(&isolation); err != nil {
		return nil, storageError(ctx)
	}
	if isolation != "read committed" {
		return nil, ErrIsolation
	}
	// A stable, namespaced hash collision only serializes unrelated users; it cannot grant access.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 110))", id); err != nil {
		return nil, storageError(ctx)
	}
	return tx, nil
}

// Purge deletes service data and remembers the deletion atomically in the caller's
// transaction. Repeated calls skip remove and succeed. The caller MUST roll back on
// any error, including a remove error (returned unchanged). remove may use only this
// transaction and must make no network calls. It receives the canonical lowercase
// UUID so its deletion keys match the lock and tombstone. Other errors are those of Guard.
func (s *Store) Purge(ctx context.Context, id string, remove func(context.Context, string) error) error {
	if remove == nil {
		return errors.New("tombstone: deletion callback required")
	}
	id, err := canonicalID(id)
	if err != nil {
		return err
	}
	alive, err := s.Guard(ctx, id)
	if err != nil {
		return err
	}
	if !alive {
		return nil
	}
	if err := remove(ctx, id); err != nil {
		return err
	}
	tx, _ := s.tx.Tx(ctx)
	if _, err := tx.Exec(ctx, "INSERT INTO tombstone_users (user_id, deleted_at) VALUES ($1, $2)", id, s.now().UTC()); err != nil {
		return storageError(ctx)
	}
	return nil
}

func storageError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return ErrStorage
}
