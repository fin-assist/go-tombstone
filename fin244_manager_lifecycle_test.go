package tombstone_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestIntegrationManagerPurgeAbnormalExitReleasesPool(t *testing.T) {
	for _, exit := range []string{"panic", "canceled", "Goexit", "nested Goexit"} {
		t.Run(exit, func(t *testing.T) {
			f := newManagerFixture(t, 1)
			if err := f.manager.Run(f.ctx, f.write); err != nil {
				t.Fatal(err)
			}
			request, cancel := context.WithCancel(f.ctx)
			defer cancel()
			var active pgx.Tx
			// A failing regression must still release the acquired transaction before pool.Close.
			t.Cleanup(func() {
				if active != nil {
					ctx, cancel := context.WithTimeout(context.Background(), time.Second)
					defer cancel()
					_ = active.Rollback(ctx)
				}
			})
			done := make(chan struct{})
			result := make(chan error, 1)
			go func() {
				defer close(done)
				defer func() {
					if p := recover(); p != nil {
						if p != "service panic" {
							result <- fmt.Errorf("unexpected panic: %v", p)
						}
					}
				}()
				result <- f.manager.Run(request, func(ctx context.Context) error {
					active, _ = f.manager.Tx(ctx)
					if err := f.store.Purge(ctx, managerUser, f.remove); err != nil {
						return err
					}
					switch exit {
					case "panic":
						panic("service panic")
					case "canceled":
						cancel()
						return ctx.Err()
					case "Goexit":
						runtime.Goexit()
					case "nested Goexit":
						return f.manager.Run(ctx, func(context.Context) error {
							runtime.Goexit()
							return nil
						})
					}
					return nil
				})
			}()
			select {
			case <-done:
			case <-f.ctx.Done():
				t.Fatal("transaction goroutine did not stop")
			}
			select {
			case err := <-result:
				if exit != "canceled" || !errors.Is(err, context.Canceled) {
					t.Fatalf("unexpected callback completion: %v", err)
				}
			default:
				if exit == "canceled" {
					t.Fatal("canceled Run did not return its error")
				}
			}
			if got := f.pool.Stat().AcquiredConns(); got != 0 {
				t.Fatalf("transaction leaked %d acquired connections in a one-connection pool", got)
			}
			f.assertState(t, 1, 0, 0)
			// Reacquiring the only connection and deleting the same UUID also proves lock cleanup.
			next, stop := context.WithTimeout(f.ctx, 2*time.Second)
			defer stop()
			if err := f.manager.Run(next, func(ctx context.Context) error {
				return f.store.Purge(ctx, managerUser, f.remove)
			}); err != nil {
				t.Fatalf("next deletion after %s: %v", exit, err)
			}
			f.assertState(t, 0, 1, 0)
		})
	}
}

func TestIntegrationManagerDeletionWriteExclusion(t *testing.T) {
	for _, first := range []string{"writer", "deletion"} {
		t.Run(first+" owns lock", func(t *testing.T) {
			f := newManagerFixture(t, 3)
			locked := make(chan struct{})
			release := make(chan struct{})
			firstDone := make(chan error, 1)
			secondDone := make(chan error, 1)
			secondPID := make(chan uint32, 1)
			// Always unblock the owner before fixture teardown, even when an assertion fails.
			defer close(release)
			go func() {
				firstDone <- f.manager.Run(f.ctx, func(ctx context.Context) error {
					if first == "writer" {
						if err := f.write(ctx); err != nil {
							return err
						}
					} else if err := f.store.Purge(ctx, managerUser, f.remove); err != nil {
						return err
					}
					close(locked)
					select {
					case <-release:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				})
			}()
			select {
			case <-locked:
			case err := <-firstDone:
				t.Fatalf("lock owner failed: %v", err)
			case <-f.ctx.Done():
				t.Fatal("lock owner did not start")
			}
			go func() {
				secondDone <- f.manager.Run(f.ctx, func(ctx context.Context) error {
					tx, _ := f.manager.Tx(ctx)
					secondPID <- tx.Conn().PgConn().PID()
					if first == "writer" {
						return f.store.Purge(ctx, managerUser, f.remove)
					}
					alive, err := f.store.Guard(ctx, managerUser)
					if err != nil {
						return err
					}
					if alive {
						return f.write(ctx)
					}
					return nil
				})
			}()
			var pid uint32
			select {
			case pid = <-secondPID:
			case <-f.ctx.Done():
				t.Fatal("contender did not start")
			}
			waitForManagerAdvisoryWait(t, f, pid)
			// The contender is demonstrably blocked in PostgreSQL, not merely unscheduled.
			release <- struct{}{}
			for _, completed := range []chan error{firstDone, secondDone} {
				select {
				case err := <-completed:
					if err != nil {
						t.Fatal(err)
					}
				case <-f.ctx.Done():
					t.Fatal("transaction did not complete after releasing advisory lock")
				}
			}
			f.assertState(t, 0, 1, 0)
		})
	}
}

func waitForManagerAdvisoryWait(t *testing.T, f *managerFixture, pid uint32) {
	t.Helper()
	ctx, cancel := context.WithTimeout(f.ctx, 3*time.Second)
	defer cancel()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		var blocked bool
		if err := f.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_locks WHERE pid=$1 AND locktype='advisory' AND NOT granted)", pid).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			return
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			t.Fatal("contender never waited for the service advisory lock")
		}
	}
}
