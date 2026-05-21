package telemetry

import (
	"context"
	"testing"
	"time"
)

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
