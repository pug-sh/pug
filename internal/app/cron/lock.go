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

// ErrLockHeld means another pass holds the job's lock and this one did nothing.
// Not a failed pass — whoever holds it is doing the work, and a caller driving a
// CronJob should map this to exit 0. It is a distinct value rather than a nil so
// that "skipped" and "ran" are tellable apart: every pass returning nil looks
// identical whether the job is healthy or its lock has been wedged for a day.
var ErrLockHeld = errors.New("cron: another pass holds the job lock")

// WithLock runs fn holding job's lock, so a pass that overruns its schedule
// cannot double up with the next one. Contention returns ErrLockHeld. See
// docs/architecture/usage.md §4 for why the lock is transaction-scoped, and for
// the one setting that can drop it mid-pass.
func WithLock(ctx context.Context, pgW *pgxpool.Pool, job Job, fn func(context.Context) error) (err error) {
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

	// The tx holds the lock while doing nothing, which is exactly what a non-zero
	// idle_in_transaction_session_timeout kills — several managed Postgres
	// providers ship one. Disabling it for this transaction only (SET LOCAL reverts
	// when the connection returns to the pool) is what makes "cannot double-meter"
	// true for the pass's whole duration rather than only until the timeout fires.
	// The rollback handler above stays as the backstop for whatever else can drop
	// the connection.
	if _, err := tx.Exec(ctx, "set local idle_in_transaction_session_timeout = 0"); err != nil {
		slog.ErrorContext(ctx, "failed to disable the idle-in-transaction timeout for the cron lock",
			slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return err
	}

	acquired, err := dbwrite.New(tx).TryCronLock(ctx, int64(job.Key))
	if err != nil {
		slog.ErrorContext(ctx, "failed to take the cron lock", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return err
	}
	if !acquired {
		slog.WarnContext(ctx, "another pass holds the cron lock; skipping",
			slog.String("job", job.Name), slog.Int64("lock_key", int64(job.Key)))
		return ErrLockHeld
	}

	// fn writes through its own pool, not this tx: the lock is a mutual-exclusion
	// token, not a unit of work.
	return fn(ctx)
}
