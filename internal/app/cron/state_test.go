package cron_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pug-sh/pug/internal/app/cron"
	"github.com/pug-sh/pug/internal/testutil"
)

func TestMain(m *testing.M) { testutil.Main(m) }

func TestStateRoundTrips(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	ctx := t.Context()
	s := cron.NewState(pg.PgRO, pg.PgW, cron.JobUsage)

	last, err := s.LastRun(ctx, "prune")
	if err != nil {
		t.Fatalf("LastRun: %v", err)
	}
	if !last.IsZero() {
		t.Errorf("LastRun = %s on a fresh database, want zero", last)
	}

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	if err := s.MarkRun(ctx, "prune", now); err != nil {
		t.Fatalf("MarkRun: %v", err)
	}
	got, err := s.LastRun(ctx, "prune")
	if err != nil {
		t.Fatalf("LastRun after MarkRun: %v", err)
	}
	if !got.Equal(now) {
		t.Errorf("LastRun = %s, want %s", got, now)
	}
}

// The job name is bound at construction and prefixed onto every key, so two jobs
// picking the same task name do not share a row.
func TestStateIsNamespacedByJob(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	ctx := t.Context()
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	if err := cron.NewState(pg.PgRO, pg.PgW, cron.JobUsage).MarkRun(ctx, "prune", now); err != nil {
		t.Fatalf("MarkRun: %v", err)
	}

	other, err := cron.NewState(pg.PgRO, pg.PgW, cron.Job{Name: "retention"}).LastRun(ctx, "prune")
	if err != nil {
		t.Fatalf("LastRun: %v", err)
	}
	if !other.IsZero() {
		t.Errorf("a second job read %s from another job's task row", other)
	}
}

// A failed read and a task that never ran both come back zero, so only the error
// tells them apart.
func TestStateSurfacesAFailedReadAndWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	s := cron.NewState(pg.PgRO, pg.PgW, cron.JobUsage)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := s.LastRun(ctx, "prune"); !errors.Is(err, context.Canceled) {
		t.Errorf("LastRun err = %v, want context.Canceled", err)
	}
	if err := s.MarkRun(ctx, "prune", time.Now()); !errors.Is(err, context.Canceled) {
		t.Errorf("MarkRun err = %v, want context.Canceled", err)
	}
}
