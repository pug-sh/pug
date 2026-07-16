package testutil

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/testcontainers/testcontainers-go"

	goredis "github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go/modules/redis"
)

// testRedisImage pins the Redis image used as the cache test double. Dev/prod run
// Dragonfly (infra/dev/docker-compose.yaml), which speaks the Redis wire protocol;
// Redis stands in for it in tests via the testcontainers redis module.
const testRedisImage = "redis@sha256:d146f83b1e0f02fc27c26a50cee39338c736674c5959db84363e6ae3cd9e02d2" // 8.6.3-alpine

// redisDBCount is Redis' default number of logical databases. Tests are handed
// one each, so they share a container without sharing keyspace.
const redisDBCount = 16

// TestRedis holds a client onto one test's private Redis database.
type TestRedis struct {
	URL    string
	Client *goredis.Client
}

// sharedRedis is the single container backing every test in the package.
type sharedRedis struct {
	addr string
}

var redisContainer = &lazyContainer[sharedRedis]{kind: "redis", start: startRedis}

// redisDBSeq hands out logical database indexes. It wraps at redisDBCount, so
// an index can be reused by a later test in the same package — hence the flush
// on the way in as well as the way out.
var redisDBSeq atomic.Uint64

// SetupRedis starts (or reuses) the package's Redis container and returns a
// client scoped to a logical database private to this test. Cleanup is
// registered via t.Cleanup; the container is torn down by Main.
func SetupRedis(t *testing.T) *TestRedis {
	t.Helper()
	// Scoped to the test — see the note in SetupPostgres for why the shared
	// container and the cleanup below use context.Background instead.
	ctx := t.Context()

	addr := redisContainer.get(t).addr
	db := int(redisDBSeq.Add(1)-1) % redisDBCount
	url := fmt.Sprintf("redis://%s/%d", addr, db)

	opts, err := goredis.ParseURL(url)
	if err != nil {
		t.Fatalf("testutil: parse redis URL: %v", err)
	}

	client := goredis.NewClient(opts)
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		t.Fatalf("testutil: ping redis: %v", err)
	}
	if err := client.FlushDB(ctx).Err(); err != nil {
		_ = client.Close()
		t.Fatalf("testutil: flush redis db %d: %v", db, err)
	}

	t.Cleanup(func() {
		// Background, not t.Context: the test's context is already cancelled once
		// cleanups run, so the flush would never be sent and this index would hand
		// stale keys to whichever later test recycles it. The timeout backstops a
		// wedged connection hanging the flush — and with it the rest of the
		// sequential package — until go test -timeout (cleanupDropTimeout).
		dropCtx, cancel := context.WithTimeout(context.Background(), cleanupDropTimeout)
		defer cancel()
		if err := client.FlushDB(dropCtx).Err(); err != nil {
			t.Errorf("testutil: flush redis db %d: %v", db, err)
		}
		_ = client.Close()
	})

	return &TestRedis{URL: url, Client: client}
}

func startRedis() (_ *sharedRedis, err error) {
	// Background, not the triggering test's t.Context: this container is shared by
	// the whole package and outlives whichever test started it.
	ctx := context.Background()

	ctr, err := redis.Run(ctx, testRedisImage)
	// Run hands back a created-but-never-ready container alongside its error when
	// the readiness probe times out, so every failure path below has to terminate
	// it. TerminateContainer tolerates a nil container.
	defer func() {
		if err != nil {
			err = errors.Join(err, testcontainers.TerminateContainer(ctr))
		}
	}()
	if err != nil {
		return nil, fmt.Errorf("start container: %w", err)
	}

	host, err := ctr.Host(ctx)
	if err != nil {
		return nil, fmt.Errorf("container host: %w", err)
	}

	port, err := ctr.MappedPort(ctx, "6379")
	if err != nil {
		return nil, fmt.Errorf("container port: %w", err)
	}

	teardowns.add(func() error {
		if err := testcontainers.TerminateContainer(ctr); err != nil {
			return fmt.Errorf("terminate redis container: %w", err)
		}
		return nil
	})

	return &sharedRedis{addr: fmt.Sprintf("%s:%s", host, port.Port())}, nil
}
