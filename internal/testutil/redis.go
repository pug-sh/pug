package testutil

import (
	"context"
	"fmt"
	"testing"

	goredis "github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

const testDragonflyImage = "docker.dragonflydb.io/dragonflydb/dragonfly@sha256:243bf004df5e137d9432c35367d7c86e6d2b5bcb6700be4b6397fcb9306b794b" // v1.38.0

// TestRedis holds a Dragonfly testcontainer for testing.
type TestRedis struct {
	container testcontainers.Container
	URL       string
	Client    *goredis.Client
}

// SetupRedis starts a Dragonfly container and returns a connected client.
// Cleanup is registered via t.Cleanup.
func SetupRedis(t *testing.T) *TestRedis {
	t.Helper()
	ctx := context.Background()

	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        testDragonflyImage,
			ExposedPorts: []string{"6379/tcp"},
			WaitingFor:   wait.ForListeningPort("6379/tcp"),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("testutil: start dragonfly container: %v", err)
	}

	host, err := ctr.Host(ctx)
	if err != nil {
		_ = testcontainers.TerminateContainer(ctr)
		t.Fatalf("testutil: get dragonfly host: %v", err)
	}

	port, err := ctr.MappedPort(ctx, "6379")
	if err != nil {
		_ = testcontainers.TerminateContainer(ctr)
		t.Fatalf("testutil: get dragonfly port: %v", err)
	}

	url := fmt.Sprintf("redis://%s:%s", host, port.Port())

	opts, err := goredis.ParseURL(url)
	if err != nil {
		_ = testcontainers.TerminateContainer(ctr)
		t.Fatalf("testutil: parse redis URL: %v", err)
	}

	client := goredis.NewClient(opts)
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		_ = testcontainers.TerminateContainer(ctr)
		t.Fatalf("testutil: ping dragonfly: %v", err)
	}

	tr := &TestRedis{
		container: ctr,
		URL:       url,
		Client:    client,
	}

	t.Cleanup(func() {
		_ = client.Close()
		if err := testcontainers.TerminateContainer(ctr); err != nil {
			fmt.Printf("testutil: terminate dragonfly container: %v\n", err)
		}
	})

	return tr
}
