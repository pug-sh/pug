package cron

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
)

// WithLock runs fn holding key, so a pass that overruns its schedule cannot
// double up with the next one. Contention returns nil: whoever holds the lock is
// doing this work. See docs/architecture/usage.md §4 for why the lock is
// transaction-scoped.
func WithLock(ctx context.Context, pgW *pgxpool.Pool, key LockKey, fn func(context.Context) error) (err error) {
	tx, err := pgW.Begin(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to begin the cron lock tx", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return err
	}
	// The tx sits idle for the whole pass, so an idle_in_transaction_session_timeout
	// can kill it and drop the lock while fn still runs. Joining the rollback error
	// is what makes that a failed pass rather than a log line.
	defer func() {
		rollbackErr := tx.Rollback(ctx)
		if rollbackErr == nil || errors.Is(rollbackErr, pgx.ErrTxClosed) || errors.Is(rollbackErr, context.Canceled) {
			return
		}
		slog.ErrorContext(ctx, "failed rolling back the cron lock tx", slogx.Error(rollbackErr))
		telemetry.RecordError(ctx, rollbackErr)
		err = errors.Join(err, rollbackErr)
	}()

	acquired, err := dbwrite.New(tx).TryCronLock(ctx, int64(key))
	if err != nil {
		slog.ErrorContext(ctx, "failed to take the cron lock", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return err
	}
	if !acquired {
		slog.WarnContext(ctx, "another pass holds the cron lock; skipping", slog.Int64("lock_key", int64(key)))
		return nil
	}

	// fn writes through its own pool, not this tx: the lock is a mutual-exclusion
	// token, not a unit of work.
	return fn(ctx)
}
