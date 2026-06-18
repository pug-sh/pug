package testutil

import (
	"context"
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

// TestNATS holds a NATS testcontainer for testing.
type TestNATS struct {
	container *nats.NATSContainer
	URL       string
}

// SetupNATS starts a NATS container with JetStream enabled.
// It registers a cleanup function on the test to terminate the container.
// It also runs NATS migrations to create streams and consumers.
func SetupNATS(t *testing.T) *TestNATS {
	t.Helper()

	ctx := context.Background()

	ctr, err := nats.Run(ctx, testNATSImage)
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

	// Run NATS migrations (create streams and consumers).
	if err := tn.runMigrations(ctx, t); err != nil {
		_ = testcontainers.TerminateContainer(ctr)
		t.Fatalf("testutil: run nats migrations: %v", err)
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

func (tn *TestNATS) runMigrations(ctx context.Context, _ *testing.T) error {
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
