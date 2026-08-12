package cron

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
)

// State is one job's slice of cron_state — every pass is a fresh process, so a
// job holding some work to daily has nowhere else to keep that timestamp. The job
// name is prefixed onto every key, so two jobs cannot collide on a task name.
type State struct {
	read  *dbread.Queries
	write *dbwrite.Queries
	job   string
}

func NewState(pgRO, pgW *pgxpool.Pool, job string) *State {
	return &State{read: dbread.New(pgRO), write: dbwrite.New(pgW), job: job}
}

func (s *State) key(task string) string { return s.job + "." + task }

// LastRun reports when task last completed, zero if never.
func (s *State) LastRun(ctx context.Context, task string) (time.Time, error) {
	row, err := s.read.GetCronState(ctx, s.key(task))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return time.Time{}, nil
		}
		slog.ErrorContext(ctx, "failed to read cron state", slogx.Error(err), slog.String("task", s.key(task)))
		telemetry.RecordError(ctx, err)
		return time.Time{}, err
	}
	return row.LastRunAt.Time, nil
}

// MarkRun records a completed task. Call it only on success — a failed task that
// stamped itself would not be retried until its interval came round again.
func (s *State) MarkRun(ctx context.Context, task string, at time.Time) error {
	err := s.write.MarkCronRun(ctx, dbwrite.MarkCronRunParams{
		Task:      s.key(task),
		LastRunAt: postgres.NewTimestamptz(at),
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to mark a cron run", slogx.Error(err), slog.String("task", s.key(task)))
		telemetry.RecordError(ctx, err)
	}
	return err
}
