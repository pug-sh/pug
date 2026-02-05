package commands

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/fivebitsio/cotton/internal/deps/nats"
	"github.com/joho/godotenv"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/spf13/cobra"
)

// TODO: For self-hosting, move stream/consumer initialization to app startup instead of a
// separate migrate command. This reduces setup friction — users currently need JetStream
// enabled with specific storage limits, and the 50GB-per-stream defaults break local setups.
// Consider: auto-create on startup, unified `cotton migrate` for all DBs, env-aware defaults.

// NATSInitializer handles NATS initialization operations
type NATSInitializer struct {
	client *nats.NATSClient
}

// NATSMigrateCmd represents the nats migrate command
var NATSMigrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Initialize NATS streams and consumers",
	Long:  `Initialize NATS streams and consumers by creating them in the NATS cluster.`,
	Run: func(cmd *cobra.Command, args []string) {
		ctx := context.Background()

		if err := godotenv.Load(); err != nil {
			slog.Debug("No .env file found")
		}

		client, err := nats.New(ctx)
		if err != nil {
			slog.Error("Failed to create NATS client", slog.Any("err", err))
			os.Exit(1)
		}

		initializer := NewNATSInitializer(client)
		defer initializer.client.Close()

		if err := initializer.Initialize(); err != nil {
			slog.Error("Failed to initialize NATS", slog.Any("err", err))
			os.Exit(1)
		}

		slog.Info("NATS initialization completed successfully")
	},
}

// NewNATSInitializer creates a new NATSInitializer instance
func NewNATSInitializer(client *nats.NATSClient) *NATSInitializer {
	return &NATSInitializer{
		client: client,
	}
}

func (n *NATSInitializer) Initialize() error {
	slog.Info("Starting NATS initialization",
		slog.String("nats_url", n.client.GetConfig().NATSUrl))

	streamConfig, err := n.client.ReadStreamConfig()
	if err != nil {
		return fmt.Errorf("failed to read stream configuration: %w", err)
	}

	consumerConfig, err := n.client.ReadConsumerConfig()
	if err != nil {
		return fmt.Errorf("failed to read consumer configuration: %w", err)
	}

	if err := n.createStreams(streamConfig); err != nil {
		return fmt.Errorf("failed to create streams: %w", err)
	}

	if err := n.createConsumers(consumerConfig); err != nil {
		return fmt.Errorf("failed to create consumers: %w", err)
	}

	return nil
}

func (n *NATSInitializer) createStreams(streams []nats.StreamConfig) error {
	for _, streamConfig := range streams {
		slog.Info("Creating stream",
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
		_, err := js.CreateStream(context.Background(), cfg)
		if err != nil {
			if strings.Contains(err.Error(), "stream name already in use") {
				slog.Info("Stream already exists", slog.String("name", streamConfig.Name))

				// Update the existing stream with new configuration
				_, err = js.UpdateStream(context.Background(), cfg)
				if err != nil {
					return fmt.Errorf("failed to update stream %s: %w", streamConfig.Name, err)
				}
				slog.Info("Updated existing stream", slog.String("name", streamConfig.Name))
			} else {
				return fmt.Errorf("failed to create stream %s: %w", streamConfig.Name, err)
			}
		} else {
			slog.Info("Created stream successfully", slog.String("name", streamConfig.Name))
		}
	}

	return nil
}

func (n *NATSInitializer) createConsumers(consumers []nats.ConsumerConfig) error {
	for _, consumerConfig := range consumers {
		slog.Info("Creating consumer",
			slog.String("name", consumerConfig.Name),
			slog.String("stream", consumerConfig.StreamName))

		cfg := jetstream.ConsumerConfig{
			Name:       consumerConfig.DurableName,
			Durable:    consumerConfig.DurableName,
			AckPolicy:  jetstream.AckExplicitPolicy, // Using AckExplicit as AckPolicy
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

		js := n.client.GetJetStream()
		_, err := js.CreateOrUpdateConsumer(context.Background(), consumerConfig.StreamName, cfg)
		if err != nil {
			return fmt.Errorf("failed to create consumer %s for stream %s: %w", consumerConfig.Name, consumerConfig.StreamName, err)
		}

		slog.Info("Created consumer successfully",
			slog.String("name", consumerConfig.Name),
			slog.String("stream", consumerConfig.StreamName))
	}

	return nil
}
