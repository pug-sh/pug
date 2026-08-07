package testutil

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	natsdeps "github.com/pug-sh/pug/internal/deps/nats"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/nats"
	"gopkg.in/yaml.v3"
)

const testNATSImage = "nats@sha256:ea17b9b7f74279b9239cf65e5786c0133e9a7c353bf218d29004abf2e7a33181" // 2.14.1-alpine

const (
	testStreamMaxBytes = 128 * 1024 * 1024
	testStreamMaxAge   = 24 * time.Hour
)

// TestNATS holds the connection URL for the package's NATS container.
type TestNATS struct {
	URL string
}

// sharedNATS is the single container backing every test in the package.
type sharedNATS struct {
	url string
}

var natsContainer = &lazyContainer[sharedNATS]{kind: "nats", start: startNATS}

// SetupNATS returns a URL onto the package's NATS container with the configured
// streams and consumers freshly created. The container is started once per test
// binary and torn down by Main.
//
// Unlike Postgres and ClickHouse, JetStream has no per-test namespace to hand
// out, so isolation comes from rebuilding the streams on every call: dropping a
// stream drops its messages and its consumers' ack state along with it, which
// is the state a test could otherwise inherit from the one before it.
func SetupNATS(t *testing.T) *TestNATS {
	t.Helper()
	// Scoped to the test — see the note in SetupPostgres for why the shared
	// container uses context.Background instead.
	ctx := t.Context()

	tn := &TestNATS{URL: natsContainer.get(t).url}

	if err := tn.resetJetStream(ctx); err != nil {
		t.Fatalf("testutil: reset jetstream: %v", err)
	}
	if err := tn.runMigrations(ctx); err != nil {
		t.Fatalf("testutil: run nats migrations: %v", err)
	}

	return tn
}

func startNATS() (_ *sharedNATS, err error) {
	// Background, not the triggering test's t.Context: this container is shared by
	// the whole package and outlives whichever test started it.
	ctx := context.Background()

	ctr, err := nats.Run(ctx, testNATSImage)
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

	url, err := ctr.ConnectionString(ctx)
	if err != nil {
		return nil, fmt.Errorf("connection string: %w", err)
	}

	// Wait for NATS to be fully ready by attempting a connection with retries.
	if err = waitForNATS(url, 30*time.Second); err != nil {
		return nil, fmt.Errorf("wait for nats ready: %w", err)
	}

	teardowns.add(func() error {
		if err := testcontainers.TerminateContainer(ctr); err != nil {
			return fmt.Errorf("terminate nats container: %w", err)
		}
		return nil
	})

	return &sharedNATS{url: url}, nil
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

// resetJetStream drops every stream left behind by an earlier test in this
// package, along with the consumers that hang off them. runMigrations recreates
// both.
func (tn *TestNATS) resetJetStream(ctx context.Context) error {
	nc, err := natsgo.Connect(tn.URL)
	if err != nil {
		return fmt.Errorf("connect to nats: %w", err)
	}
	defer nc.Close()

	js, err := jetstream.New(nc)
	if err != nil {
		return fmt.Errorf("create jetstream: %w", err)
	}

	lister := js.StreamNames(ctx)
	var names []string
	for name := range lister.Name() {
		names = append(names, name)
	}
	if err := lister.Err(); err != nil {
		return fmt.Errorf("list streams: %w", err)
	}

	for _, name := range names {
		if err := js.DeleteStream(ctx, name); err != nil {
			return fmt.Errorf("delete stream %s: %w", name, err)
		}
	}

	return nil
}

func (tn *TestNATS) runMigrations(ctx context.Context) error {
	nc, err := natsgo.Connect(tn.URL)
	if err != nil {
		return fmt.Errorf("connect to nats: %w", err)
	}
	defer nc.Close()

	js, err := jetstream.New(nc)
	if err != nil {
		return fmt.Errorf("create jetstream: %w", err)
	}

	streams, err := readStreamConfig()
	if err != nil {
		return fmt.Errorf("read stream config: %w", err)
	}

	for _, stream := range streams {
		slog.InfoContext(ctx, "testutil: creating stream",
			slog.String("name", stream.Name))

		cfg := testStreamConfig(stream)

		if _, err := js.CreateOrUpdateStream(ctx, cfg); err != nil {
			return fmt.Errorf("create stream %s: %w", stream.Name, err)
		}
	}

	consumers, err := readConsumerConfig()
	if err != nil {
		return fmt.Errorf("read consumer config: %w", err)
	}

	for _, consumer := range consumers {
		slog.InfoContext(ctx, "testutil: creating consumer",
			slog.String("name", consumer.Name),
			slog.String("stream", consumer.StreamName))

		cfg := jetstream.ConsumerConfig{
			Name:       consumer.DurableName,
			Durable:    consumer.DurableName,
			AckPolicy:  jetstream.AckExplicitPolicy,
			MaxDeliver: consumer.MaxDeliver,
		}

		if consumer.FilterSubject != "" {
			cfg.FilterSubject = consumer.FilterSubject
		}

		if _, err := js.CreateOrUpdateConsumer(ctx, consumer.StreamName, cfg); err != nil {
			return fmt.Errorf("create consumer %s: %w", consumer.Name, err)
		}
	}

	return nil
}

func testStreamConfig(stream natsdeps.StreamConfig) jetstream.StreamConfig {
	maxBytes := stream.MaxBytes
	if maxBytes <= 0 || maxBytes > testStreamMaxBytes {
		maxBytes = testStreamMaxBytes
	}

	maxAge := stream.MaxAge
	if maxAge <= 0 || maxAge > testStreamMaxAge {
		maxAge = testStreamMaxAge
	}

	return jetstream.StreamConfig{
		Name:         stream.Name,
		Description:  stream.Description,
		Subjects:     stream.Subjects,
		Retention:    testRetentionPolicy(stream.RetentionPolicy),
		MaxConsumers: stream.MaxConsumers,
		MaxMsgs:      stream.MaxMsgs,
		MaxBytes:     maxBytes,
		MaxAge:       maxAge,
		Discard:      testDiscardPolicy(stream.Discard),
		// CI test containers do not have production-scale disk budgets.
		Storage:  jetstream.MemoryStorage,
		Replicas: 1,
	}
}

func testRetentionPolicy(policy string) jetstream.RetentionPolicy {
	switch strings.ToLower(policy) {
	case "interest":
		return jetstream.InterestPolicy
	case "workqueue":
		return jetstream.WorkQueuePolicy
	default:
		return jetstream.LimitsPolicy
	}
}

func testDiscardPolicy(policy string) jetstream.DiscardPolicy {
	if strings.EqualFold(policy, "new") {
		return jetstream.DiscardNew
	}
	return jetstream.DiscardOld
}

func readStreamConfig() ([]natsdeps.StreamConfig, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return nil, fmt.Errorf("unable to determine source file path")
	}
	dir := filepath.Join(filepath.Dir(thisFile), "..", "..", "schema", "nats")
	data, err := os.ReadFile(filepath.Join(dir, "streams.yaml"))
	if err != nil {
		return nil, fmt.Errorf("read streams config: %w", err)
	}

	var config struct {
		Streams []natsdeps.StreamConfig `yaml:"streams"`
	}

	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parse streams config: %w", err)
	}

	return config.Streams, nil
}

func readConsumerConfig() ([]natsdeps.ConsumerConfig, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return nil, fmt.Errorf("unable to determine source file path")
	}
	dir := filepath.Join(filepath.Dir(thisFile), "..", "..", "schema", "nats")
	data, err := os.ReadFile(filepath.Join(dir, "consumers.yaml"))
	if err != nil {
		return nil, fmt.Errorf("read consumers config: %w", err)
	}

	var config struct {
		Consumers []natsdeps.ConsumerConfig `yaml:"consumers"`
	}

	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parse consumers config: %w", err)
	}

	return config.Consumers, nil
}
