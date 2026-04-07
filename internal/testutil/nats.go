package testutil

import (
	"context"
	"fmt"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"
)

// TestNATS holds a NATS testcontainer for testing.
type TestNATS struct {
	container *tcnats.NATSContainer
	URL       string
}

// SetupNATS starts a NATS container with JetStream enabled.
// It registers a cleanup function on the test to terminate the container.
func SetupNATS(t *testing.T) *TestNATS {
	t.Helper()

	ctx := context.Background()

	ctr, err := tcnats.Run(ctx, "nats:2.12-alpine")
	if err != nil {
		t.Fatalf("testutil: start nats container: %v", err)
	}

	url, err := ctr.ConnectionString(ctx)
	if err != nil {
		_ = testcontainers.TerminateContainer(ctr)
		t.Fatalf("testutil: get nats connection string: %v", err)
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
