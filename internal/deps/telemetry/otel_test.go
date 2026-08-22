package telemetry

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

func TestInstallStdoutLogHandler_wrapsCorrelationHandler(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	installStdoutLogHandler()
	if _, ok := slog.Default().Handler().(*correlationHandler); !ok {
		t.Fatalf("handler type = %T, want *correlationHandler", slog.Default().Handler())
	}
}

func TestShutdownContextPreservesDeadline(t *testing.T) {
	parent := context.Background()
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()

	shutdownCtx, shutdownCancel := shutdownContext(ctx)
	defer shutdownCancel()

	if shutdownCtx != ctx {
		t.Fatal("expected shutdown context to reuse caller context when deadline exists")
	}

	deadline, ok := shutdownCtx.Deadline()
	if !ok {
		t.Fatal("expected shutdown context deadline to be preserved")
	}
	parentDeadline, _ := ctx.Deadline()
	if !deadline.Equal(parentDeadline) {
		t.Fatalf("expected deadline %v, got %v", parentDeadline, deadline)
	}
}

func TestShutdownContextAddsDefaultTimeoutWithoutDeadline(t *testing.T) {
	ctx := context.Background()

	before := time.Now()
	shutdownCtx, cancel := shutdownContext(ctx)
	defer cancel()

	deadline, ok := shutdownCtx.Deadline()
	if !ok {
		t.Fatal("expected shutdown context to add a fallback deadline")
	}
	remaining := time.Until(deadline)
	if remaining > shutdownTimeout || remaining < shutdownTimeout-time.Second {
		t.Fatalf("expected remaining timeout near %v, got %v", shutdownTimeout, remaining)
	}
	if deadline.Before(before) {
		t.Fatal("expected shutdown deadline to be in the future")
	}
}

func TestOnceShutdownUsesFirstContext(t *testing.T) {
	first, cancelFirst := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelFirst()

	second, cancelSecond := context.WithTimeout(context.Background(), time.Second)
	defer cancelSecond()

	var seen context.Context
	shutdown := onceShutdown(func(ctx context.Context) error {
		seen = ctx
		return nil
	})

	if err := shutdown(first); err != nil {
		t.Fatalf("first shutdown call: %v", err)
	}
	if err := shutdown(second); err != nil {
		t.Fatalf("second shutdown call: %v", err)
	}
	if seen != first {
		t.Fatal("expected onceShutdown to execute with the first caller context")
	}
}

func TestShutdownOnExitDropsCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// The callback is the only point the shutdown context is still live, so the
	// assertions sit there.
	called := false
	ShutdownOnExit(ctx, func(c context.Context) error {
		called = true
		if err := c.Err(); err != nil {
			t.Errorf("shutdown context inherited cancellation: %v", err)
		}
		deadline, ok := c.Deadline()
		if !ok {
			t.Error("expected a shutdown deadline")
			return nil
		}
		if budget := time.Until(deadline); budget <= 0 || budget > exitShutdownTimeout {
			t.Errorf("deadline budget = %v, want (0, %v]", budget, exitShutdownTimeout)
		}
		return nil
	})

	if !called {
		t.Fatal("shutdown func was not called")
	}
}
