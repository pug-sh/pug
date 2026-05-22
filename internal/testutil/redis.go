package testutil

import (
	"context"
	"fmt"
	"testing"

	goredis "github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go/modules/redis"
)

// testRedisImage pins the Redis image used as the cache test double. Dev/prod run
// Dragonfly (infra/dev/docker-compose.yaml), which speaks the Redis wire protocol;
// Redis stands in for it in tests via the testcontainers redis module.
const testRedisImage = "redis@sha256:d146f83b1e0f02fc27c26a50cee39338c736674c5959db84363e6ae3cd9e02d2" // 8.6.3-alpine

// TestRedis holds a Redis testcontainer for testing.
type TestRedis struct {
	container *redis.RedisContainer
	URL       string
	Client    *goredis.Client
}

// SetupRedis starts a Redis container and returns a connected client.
// Cleanup is registered via t.Cleanup.
func SetupRedis(t *testing.T) *TestRedis {
	t.Helper()
	ctx := context.Background()

	ctr, err := redis.Run(ctx, testRedisImage)
	if err != nil {
		t.Fatalf("testutil: start redis container: %v", err)
	}

	host, err := ctr.Host(ctx)
	if err != nil {
		_ = ctr.Terminate(ctx)
		t.Fatalf("testutil: get redis host: %v", err)
	}

	port, err := ctr.MappedPort(ctx, "6379")
	if err != nil {
		_ = ctr.Terminate(ctx)
		t.Fatalf("testutil: get redis port: %v", err)
	}

	url := fmt.Sprintf("redis://%s:%s", host, port.Port())

	opts, err := goredis.ParseURL(url)
	if err != nil {
		_ = ctr.Terminate(ctx)
		t.Fatalf("testutil: parse redis URL: %v", err)
	}

	client := goredis.NewClient(opts)
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		_ = ctr.Terminate(ctx)
		t.Fatalf("testutil: ping redis: %v", err)
	}

	tr := &TestRedis{
		container: ctr,
		URL:       url,
		Client:    client,
	}

	t.Cleanup(func() {
		_ = client.Close()
		if err := ctr.Terminate(ctx); err != nil {
			fmt.Printf("testutil: terminate redis container: %v\n", err)
		}
	})

	return tr
}
