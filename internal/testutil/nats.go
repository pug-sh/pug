package testutil

import (
	"context"
	"fmt"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/nats"
)

// TestNATS holds a NATS testcontainer for testing.
type TestNATS struct {
	container *nats.NATSContainer
	URL       string
}

// SetupNATS starts a NATS container with JetStream enabled.
// It registers a cleanup function on the test to terminate the container.
func SetupNATS(t *testing.T) *TestNATS {
	t.Helper()

	ctx := context.Background()

	ctr, err := nats.Run(ctx, "nats:2.12-alpine")
	if err != nil {
		t.Fatalf("testutil: start nats container: %v", err)
	}

	url, err := ctr.ConnectionString(ctx)
	if err != nil {
		_ = testcontainers.TerminateContainer(ctr)
		t.Fatalf("testutil: get nats connection string: %v", err)
	}

	// Wait for NATS to be fully ready by attempting a connection with retries.
	if err := waitForNATS(url, 30*time.Second); err != nil {
		_ = testcontainers.TerminateContainer(ctr)
		t.Fatalf("testutil: wait for nats ready: %v", err)
	}

	tn := &TestNATS{
		container: ctr,
		URL:       url,
	}

	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(ctr); err != nil {
			fmt.Printf("testutil: terminate nats container: %v\n", err)
		}
	})

	return tn
}

func waitForNATS(url string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			nc, err := natsgo.Connect(url, natsgo.MaxReconnects(1))
			if err == nil {
				nc.Close()
				return nil
			}
		}
	}
}
