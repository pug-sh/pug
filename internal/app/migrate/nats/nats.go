package nats

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	natsdeps "github.com/fivebitsio/cotton/internal/deps/nats"
	"github.com/nats-io/nats.go/jetstream"
)

// TODO: For self-hosting, move stream/consumer initialization to app startup instead of a
// separate migrate command. This reduces setup friction — users currently need JetStream
// enabled with specific storage limits, and the 50GB-per-stream defaults break local setups.
// Consider: auto-create on startup, unified `cotton migrate` for all DBs, env-aware defaults.

type initializer struct {
	client *natsdeps.NATSClient
}

func Run(ctx context.Context) error {
	client, err := natsdeps.New(ctx)
	if err != nil {
		return err
	}
	defer client.Close()

	init := &initializer{client: client}
	if err := init.run(ctx); err != nil {
		return err
	}

	slog.InfoContext(ctx, "NATS initialization completed successfully")
	return nil
}

func (n *initializer) run(ctx context.Context) error {
	slog.InfoContext(ctx, "Starting NATS initialization",
		slog.String("nats_url", n.client.GetConfig().NATSUrl))

	streamConfig, err := n.client.ReadStreamConfig()
	if err != nil {
		return fmt.Errorf("failed to read stream configuration: %w", err)
	}

	consumerConfig, err := n.client.ReadConsumerConfig()
	if err != nil {
		return fmt.Errorf("failed to read consumer configuration: %w", err)
	}

	if err := n.createStreams(ctx, streamConfig); err != nil {
		return fmt.Errorf("failed to create streams: %w", err)
	}

	if err := n.createConsumers(ctx, consumerConfig); err != nil {
		return fmt.Errorf("failed to create consumers: %w", err)
	}

	return nil
}

func (n *initializer) createStreams(ctx context.Context, streams []natsdeps.StreamConfig) error {
	for _, streamConfig := range streams {
		slog.InfoContext(ctx, "Creating stream",
			slog.String("name", streamConfig.Name),
			slog.Any("subjects", streamConfig.Subjects))

		// Convert retention policy string to jetstream.RetentionPolicy
		var retention jetstream.RetentionPolicy
		switch strings.ToLower(streamConfig.RetentionPolicy) {
		case "limits":
			retention = jetstream.LimitsPolicy
		case "interest":
			retention = jetstream.InterestPolicy
		case "workqueue":
			retention = jetstream.WorkQueuePolicy
		default:
			retention = jetstream.LimitsPolicy // default
		}

		// Convert storage type string to jetstream.StorageType
		var storage jetstream.StorageType
		switch strings.ToLower(streamConfig.Storage) {
		case "file":
			storage = jetstream.FileStorage
		case "memory":
			storage = jetstream.MemoryStorage
		default:
			storage = jetstream.FileStorage // default
		}

		cfg := jetstream.StreamConfig{
			Name:         streamConfig.Name,
			Description:  streamConfig.Description,
			Subjects:     streamConfig.Subjects,
			Retention:    retention,
			MaxConsumers: streamConfig.MaxConsumers,
			MaxMsgs:      streamConfig.MaxMsgs,
			MaxBytes:     streamConfig.MaxBytes,
			MaxAge:       streamConfig.MaxAge,
			Storage:      storage,
			Replicas:     streamConfig.NumReplicas,
		}

		js := n.client.GetJetStream()
		_, err := js.CreateOrUpdateStream(ctx, cfg)
		if err != nil {
			return fmt.Errorf("failed to create or update stream %s: %w", streamConfig.Name, err)
		}
		slog.InfoContext(ctx, "Stream ready", slog.String("name", streamConfig.Name))
	}

	return nil
}

func (n *initializer) createConsumers(ctx context.Context, consumers []natsdeps.ConsumerConfig) error {
	for _, consumerConfig := range consumers {
		slog.InfoContext(ctx, "Creating consumer",
			slog.String("name", consumerConfig.Name),
			slog.String("stream", consumerConfig.StreamName))

		ackPolicy := jetstream.AckNonePolicy
		if *consumerConfig.AckExplicit {
			ackPolicy = jetstream.AckExplicitPolicy
		}

		cfg := jetstream.ConsumerConfig{
			Name:       consumerConfig.DurableName,
			Durable:    consumerConfig.DurableName,
			AckPolicy:  ackPolicy,
			MaxDeliver: consumerConfig.MaxDeliver,
		}

		// Set deliver policy based on configuration
		switch strings.ToLower(consumerConfig.DeliverPolicy) {
		case "all":
			cfg.DeliverPolicy = jetstream.DeliverAllPolicy
		case "last":
			cfg.DeliverPolicy = jetstream.DeliverLastPolicy
		case "new":
			cfg.DeliverPolicy = jetstream.DeliverNewPolicy
		case "by_start_time":
			cfg.DeliverPolicy = jetstream.DeliverByStartTimePolicy
			// Note: Need to set OptStartTime for this policy
		case "by_start_sequence":
			cfg.DeliverPolicy = jetstream.DeliverByStartSequencePolicy
			// Note: Need to set OptStartSeq for this policy
		default:
			cfg.DeliverPolicy = jetstream.DeliverAllPolicy // default
		}

		// Set replay policy based on configuration
		switch strings.ToLower(consumerConfig.ReplayPolicy) {
		case "instant":
			cfg.ReplayPolicy = jetstream.ReplayInstantPolicy
		case "original":
			cfg.ReplayPolicy = jetstream.ReplayOriginalPolicy
		default:
			cfg.ReplayPolicy = jetstream.ReplayInstantPolicy // default
		}

		// Set filter subject if configured
		if consumerConfig.FilterSubject != "" {
			cfg.FilterSubject = consumerConfig.FilterSubject
		}

		js := n.client.GetJetStream()
		_, err := js.CreateOrUpdateConsumer(ctx, consumerConfig.StreamName, cfg)
		if err != nil {
			return fmt.Errorf("failed to create consumer %s for stream %s: %w", consumerConfig.Name, consumerConfig.StreamName, err)
		}

		slog.InfoContext(ctx, "Created consumer successfully",
			slog.String("name", consumerConfig.Name),
			slog.String("stream", consumerConfig.StreamName))
	}

	return nil
}
