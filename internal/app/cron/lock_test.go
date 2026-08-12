package cron_test

import (
	"context"
	"errors"
	"testing"

	"github.com/pug-sh/pug/internal/app/cron"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
)

// holdLock keeps the key until the test ends, standing in for a pass running on
// another replica.
func holdLock(t *testing.T, pg *testutil.TestPostgres, key cron.LockKey) {
	t.Helper()
	ctx := t.Context()

	tx, err := pg.PgW.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder tx: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })

	acquired, err := dbwrite.New(tx).TryCronLock(ctx, int64(key))
	if err != nil {
		t.Fatalf("TryCronLock: %v", err)
	}
	if !acquired {
		t.Fatal("could not take the lock to hold it")
	}
}

func TestWithLockRunsTheWorkAndReleasesTheLock(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	ctx := t.Context()

	runs := 0
	work := func(context.Context) error {
		runs++
		return nil
	}
	if err := cron.WithLock(ctx, pg.PgW, cron.LockUsage, work); err != nil {
		t.Fatalf("WithLock: %v", err)
	}
	if runs != 1 {
		t.Fatalf("the work ran %d times, want 1", runs)
	}

	// Transaction-scoped: a session-scoped lock would ride the pooled connection
	// back and wedge every later pass.
	if err := cron.WithLock(ctx, pg.PgW, cron.LockUsage, work); err != nil {
		t.Fatalf("WithLock (second pass): %v", err)
	}
	if runs != 2 {
		t.Error("the second pass skipped: the lock outlived the pass that took it")
	}
}

// Contention is not failure, but the work must not run alongside the holder.
func TestWithLockSkipsWhenAnotherPassHoldsIt(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	holdLock(t, pg, cron.LockUsage)

	ran := false
	err := cron.WithLock(t.Context(), pg.PgW, cron.LockUsage, func(context.Context) error {
		ran = true
		return nil
	})
	if err != nil {
		t.Errorf("WithLock = %v, want nil: contention is not a failed pass", err)
	}
	if ran {
		t.Error("the work ran while another pass held the lock")
	}
}

// The work's error is the CronJob's exit code, so it has to come back intact.
func TestWithLockReturnsTheWorkError(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	sentinel := errors.New("the pass failed")

	err := cron.WithLock(t.Context(), pg.PgW, cron.LockUsage, func(context.Context) error {
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("WithLock = %v, want the work's own error", err)
	}
}

// A tx that cannot begin has to fail the pass: answering nil like contention
// would leave a broken meter looking healthy to the CronJob.
func TestWithLockFailsWhenTheTxCannotBegin(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	ran := false
	err := cron.WithLock(ctx, pg.PgW, cron.LockUsage, func(context.Context) error {
		ran = true
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("WithLock = %v, want context.Canceled", err)
	}
	if ran {
		t.Error("the work ran without ever taking the lock")
	}
}
