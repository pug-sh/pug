package correlation

import (
	"context"
	"testing"
)

func TestWithID_roundTrip(t *testing.T) {
	ctx := WithID(context.Background(), "abc123")
	if got := IDFromContext(ctx); got != "abc123" {
		t.Errorf("IDFromContext = %q, want %q", got, "abc123")
	}
}

func TestIDFromContext_emptyWhenUnset(t *testing.T) {
	if got := IDFromContext(context.Background()); got != "" {
		t.Errorf("IDFromContext = %q, want empty", got)
	}
}
