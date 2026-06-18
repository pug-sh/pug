package nats

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sethvargo/go-envconfig"
	"gopkg.in/yaml.v3"
)

// Config holds the NATS configuration
type Config struct {
	NATSUrl         string `env:"NATS_URL,default=nats://localhost:4222"`
	JWT             string `env:"NATS_JWT"`
	Seed            string `env:"NATS_SEED"`
	CredsFile       string `env:"NATS_CREDS_FILE"`
	StreamsConfig   string `env:"NATS_STREAMS_CONFIG,default=schema/nats/streams.yaml"`
	ConsumersConfig string `env:"NATS_CONSUMERS_CONFIG,default=schema/nats/consumers.yaml"`
}

type StreamConfig struct {
	Name            string        `yaml:"name"`
	Subjects        []string      `yaml:"subjects"`
	Description     string        `yaml:"description"`
	RetentionPolicy string        `yaml:"retention_policy"`
	MaxConsumers    int           `yaml:"max_consumers"`
	MaxMsgs         int64         `yaml:"max_msgs"`
	MaxBytes        int64         `yaml:"max_bytes"`
	MaxAge          time.Duration `yaml:"max_age"`
	Storage         string        `yaml:"storage"`
	// Discard is the policy when a limit (max_bytes/max_msgs) is hit: "old"
	// (default) drops the oldest messages to admit new ones, "new" rejects new
	// publishes. DLQ streams use "new" so a full DLQ fails the publish loudly
	// (surfaced via the nats.dlq_messages_total outcome=dropped metric) instead of
	// silently overwriting the oldest dead-letter evidence.
	Discard     string `yaml:"discard"`
	NumReplicas int    `yaml:"num_replicas"`
}

type ConsumerConfig struct {
	Name          string `yaml:"name"`
	StreamName    string `yaml:"stream_name"`
	DurableName   string `yaml:"durable_name"`
	FilterSubject string `yaml:"filter_subject"`
	DeliverPolicy string `yaml:"deliver_policy"`
	AckExplicit   *bool  `yaml:"ack_explicit"`
	MaxDeliver    int    `yaml:"max_deliver"`
	ReplayPolicy  string `yaml:"replay_policy"`
}

// NATSClient wraps the NATS connection and JetStream context
type NATSClient struct {
	conn      *nats.Conn
	jetStream jetstream.JetStream
	config    *Config
}

// New creates a new NATS client
func New(ctx context.Context) (*NATSClient, error) {
	var cfg Config
	if err := envconfig.Process(ctx, &cfg); err != nil {
		return nil, fmt.Errorf("failed to process NATS configuration: %w", err)
	}

	opts := []nats.Option{nats.Name("pug-nats-client")}

	if cfg.JWT != "" && cfg.Seed != "" {
		opts = append(opts, nats.UserJWTAndSeed(cfg.JWT, cfg.Seed))
	} else if cfg.CredsFile != "" {
		opts = append(opts, nats.UserCredentials(cfg.CredsFile))
	}

	conn, err := nats.Connect(cfg.NATSUrl, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to NATS: %w", err)
	}

	js, err := jetstream.New(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to create JetStream context: %w", err)
	}

	return &NATSClient{
		conn:      conn,
		jetStream: &tracedJetStream{js},
		config:    &cfg,
	}, nil
}

// IsConnected reports whether the underlying NATS connection is currently
// established. Used by readiness probes; a closed/reconnecting conn is not ready.
func (nc *NATSClient) IsConnected() bool {
	return nc.conn != nil && nc.conn.IsConnected()
}

// Close closes the NATS connection
func (nc *NATSClient) Close() {
	if nc.conn != nil {
		nc.conn.Close()
	}
}

// GetJetStream returns the JetStream context
func (nc *NATSClient) GetJetStream() jetstream.JetStream {
	return nc.jetStream
}

// IsConnected reports whether the underlying NATS connection is currently
// established. It backs worker readiness (see natsWorker.Ready); a nil connection
// reads as not connected.
func (nc *NATSClient) IsConnected() bool {
	return nc.conn != nil && nc.conn.IsConnected()
}

// GetConfig returns the NATS configuration
func (nc *NATSClient) GetConfig() *Config {
	return nc.config
}

// ReadStreamConfig reads the stream configuration from the specified file
func (nc *NATSClient) ReadStreamConfig() ([]StreamConfig, error) {
	if _, err := os.Stat(nc.config.StreamsConfig); os.IsNotExist(err) {
		return nil, fmt.Errorf("streams config file does not exist: %s", nc.config.StreamsConfig)
	}

	data, err := os.ReadFile(nc.config.StreamsConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to read streams config file: %w", err)
	}

	var config struct {
		Streams []StreamConfig `yaml:"streams"`
	}

	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse streams config: %w", err)
	}

	return config.Streams, nil
}

// ReadConsumerConfig reads the consumer configuration from the specified file
func (nc *NATSClient) ReadConsumerConfig() ([]ConsumerConfig, error) {
	if _, err := os.Stat(nc.config.ConsumersConfig); os.IsNotExist(err) {
		return nil, fmt.Errorf("consumers config file does not exist: %s", nc.config.ConsumersConfig)
	}

	data, err := os.ReadFile(nc.config.ConsumersConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to read consumers config file: %w", err)
	}

	var config struct {
		Consumers []ConsumerConfig `yaml:"consumers"`
	}

	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse consumers config: %w", err)
	}

	for i := range config.Consumers {
		if config.Consumers[i].AckExplicit == nil {
			ack := true
			config.Consumers[i].AckExplicit = &ack
		}
		if config.Consumers[i].DeliverPolicy == "" {
			config.Consumers[i].DeliverPolicy = "all"
		}
		if config.Consumers[i].ReplayPolicy == "" {
			config.Consumers[i].ReplayPolicy = "instant"
		}
	}

	return config.Consumers, nil
}

// GetConsumerConfigByName retrieves a consumer configuration by its name
func (nc *NATSClient) GetConsumerConfigByName(name string) (*ConsumerConfig, error) {
	consumers, err := nc.ReadConsumerConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to read consumer configs: %w", err)
	}

	for _, consumer := range consumers {
		if consumer.Name == name || consumer.DurableName == name {
			return &consumer, nil
		}
	}

	return nil, fmt.Errorf("consumer configuration not found for name: %s", name)
}

// GetStreamConfigByName retrieves a stream configuration by its name
func (nc *NATSClient) GetStreamConfigByName(name string) (*StreamConfig, error) {
	streams, err := nc.ReadStreamConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to read stream configs: %w", err)
	}

	for _, stream := range streams {
		if stream.Name == name {
			return &stream, nil
		}
	}

	return nil, fmt.Errorf("stream configuration not found for name: %s", name)
}

// Publish publishes a message to the specified subject
func (nc *NATSClient) Publish(ctx context.Context, subject string, data []byte) error {
	_, err := nc.jetStream.Publish(ctx, subject, data)
	return err
}
