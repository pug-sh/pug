package cron_test

import (
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
	s := cron.NewState(pg.PgRO, pg.PgW, "usage")

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

	if err := cron.NewState(pg.PgRO, pg.PgW, "usage").MarkRun(ctx, "prune", now); err != nil {
		t.Fatalf("MarkRun: %v", err)
	}

	other, err := cron.NewState(pg.PgRO, pg.PgW, "retention").LastRun(ctx, "prune")
	if err != nil {
		t.Fatalf("LastRun: %v", err)
	}
	if !other.IsZero() {
		t.Errorf("a second job read %s from another job's task row", other)
	}
}
