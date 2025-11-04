package commands

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/apache/pulsar-client-go/pulsaradmin/pkg/admin"
	"github.com/apache/pulsar-client-go/pulsaradmin/pkg/admin/config"
	"github.com/apache/pulsar-client-go/pulsaradmin/pkg/utils"
	"github.com/joho/godotenv"
	"github.com/sethvargo/go-envconfig"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v2"
)

// Config holds the Pulsar configuration
type pulsarConfig struct {
	PulsarAdminURL string `env:"PULSAR_ADMIN_URL,default=http://localhost:8080"`
	Token          string `env:"PULSAR_TOKEN"`
	Namespace      string `env:"PULSAR_NAMESPACE,default=default"`
	TopicsConfig   string `env:"PULSAR_TOPICS_CONFIG,default=schema/pulsar/topics.yaml"`
}

type TopicConfig struct {
	Name       string            `yaml:"name"`
	Type       string            `yaml:"type"`
	Partitions *int              `yaml:"partitions,omitempty"`
	Properties map[string]string `yaml:"properties,omitempty"`
	Retention  *RetentionConfig  `yaml:"retention,omitempty"`
}

type RetentionConfig struct {
	TimeInMinutes int `yaml:"timeInMinutes"`
	SizeInMB      int `yaml:"sizeInMB"`
}

type SubscriptionConfig struct {
	Name        string `yaml:"name"`
	Topic       string `yaml:"topic"`
	Type        string `yaml:"type"`
	AutoCreated bool   `yaml:"autoCreated,omitempty"`
}

// TopicSubscriptionConfig holds configurations for topics and subscriptions
type TopicSubscriptionConfig struct {
	Topics        []TopicConfig        `yaml:"topics"`
	Subscriptions []SubscriptionConfig `yaml:"subscriptions"`
}

// PulsarInitializer handles Pulsar initialization operations
type PulsarInitializer struct {
	adminClient admin.Client
	config      *pulsarConfig
}

// PulsarMigrateCmd represents the pulsar migrate command
var PulsarMigrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Initialize Pulsar topics and subscriptions",
	Long:  `Initialize Pulsar topics and subscriptions by creating them in the Pulsar cluster.`,
	Run: func(cmd *cobra.Command, args []string) {
		ctx := context.Background()

		if err := godotenv.Load(); err != nil {
			slog.Debug("No .env file found")
		}

		var cfg pulsarConfig
		if err := envconfig.Process(ctx, &cfg); err != nil {
			slog.Error("Failed to process configuration", slog.Any("err", err))
			os.Exit(1)
		}

		initializer, err := NewPulsarInitializer(&cfg)
		if err != nil {
			slog.Error("Failed to create Pulsar initializer", slog.Any("err", err))
			os.Exit(1)
		}
		defer initializer.Close()

		if err := initializer.Initialize(); err != nil {
			slog.Error("Failed to initialize Pulsar", slog.Any("err", err))
			os.Exit(1)
		}

		slog.Info("Pulsar initialization completed successfully")
	},
}

// NewPulsarInitializer creates a new PulsarInitializer instance
func NewPulsarInitializer(cfg *pulsarConfig) (*PulsarInitializer, error) {
	adminConfig := &config.Config{
		WebServiceURL: cfg.PulsarAdminURL,
		Token:         cfg.Token,
	}

	adminClient, err := admin.New(adminConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create Pulsar admin client: %w", err)
	}

	return &PulsarInitializer{
		adminClient: adminClient,
		config:      cfg,
	}, nil
}

// Close closes the admin client
func (p *PulsarInitializer) Close() {
	// The admin client doesn't have a Close method, so no cleanup required
}

func (p *PulsarInitializer) Initialize() error {
	slog.Info("Starting Pulsar initialization",
		slog.String("admin_url", p.config.PulsarAdminURL),
		slog.String("namespace", p.config.Namespace))

	topicConfig, err := p.readConfig()
	if err != nil {
		return fmt.Errorf("failed to read configuration: %w", err)
	}

	if err := p.createNamespace(); err != nil {
		return fmt.Errorf("failed to create namespace: %w", err)
	}

	if err := p.createTopics(topicConfig.Topics); err != nil {
		return fmt.Errorf("failed to create topics: %w", err)
	}

	if err := p.createSubscriptions(topicConfig.Subscriptions); err != nil {
		return fmt.Errorf("failed to create subscriptions: %w", err)
	}

	return nil
}

func (p *PulsarInitializer) readConfig() (*TopicSubscriptionConfig, error) {
	if _, err := os.Stat(p.config.TopicsConfig); os.IsNotExist(err) {
		panic(fmt.Sprintf("Config file does not exist: %s", p.config.TopicsConfig))
	}

	data, err := os.ReadFile(p.config.TopicsConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var config TopicSubscriptionConfig
	ext := strings.ToLower(filepath.Ext(p.config.TopicsConfig))

	if ext == ".yaml" || ext == ".yml" {
		if err := yaml.Unmarshal(data, &config); err != nil {
			return nil, fmt.Errorf("failed to parse YAML config: %w", err)
		}
	} else {
		return nil, fmt.Errorf("unsupported config file format: %s (only .yaml and .yml are supported)", ext)
	}

	return &config, nil
}

func (p *PulsarInitializer) createNamespace() error {
	const defaultTenant = "public"

	namespace := fmt.Sprintf("%s/%s", defaultTenant, p.config.Namespace)
	slog.Info("Creating namespace", slog.String("namespace", namespace))

	namespaces, err := p.adminClient.Namespaces().GetNamespaces(defaultTenant)
	if err != nil {
		slog.Warn("Error listing namespaces", slog.Any("err", err))
		namespaces = []string{}
	}

	namespaceExists := false
	for _, ns := range namespaces {
		if ns == namespace {
			namespaceExists = true
			break
		}
	}

	if !namespaceExists {
		err := p.adminClient.Namespaces().CreateNamespace(namespace)
		if err != nil {
			if !strings.Contains(err.Error(), "Conflict") && !strings.Contains(err.Error(), "409") {
				return fmt.Errorf("failed to create namespace %s: %w", namespace, err)
			}
			slog.Info("Namespace already exists", slog.String("namespace", namespace))
		} else {
			slog.Info("Created namespace successfully", slog.String("namespace", namespace))
		}
	} else {
		slog.Info("Namespace already exists", slog.String("namespace", namespace))
	}

	return nil
}

func (p *PulsarInitializer) createTopics(topics []TopicConfig) error {
	const defaultTenant = "public"
	namespaceName, err := utils.GetNameSpaceName(defaultTenant, p.config.Namespace)
	if err != nil {
		return fmt.Errorf("failed to create namespace name: %w", err)
	}

	for _, topicConfig := range topics {
		var partitions int
		if topicConfig.Partitions != nil {
			partitions = *topicConfig.Partitions
		} else {
			partitions = 0
		}

		topicNameStr := fmt.Sprintf("persistent://%s/%s", namespaceName.String(), topicConfig.Name)
		topicName, err := utils.GetTopicName(topicNameStr)
		if err != nil {
			return fmt.Errorf("failed to create topic name for %s: %w", topicNameStr, err)
		}

		slog.Info("Creating topic",
			slog.String("topic", topicNameStr),
			slog.Int("partitions", partitions))

		err = p.adminClient.Topics().Create(*topicName, partitions)
		if err != nil {
			if !strings.Contains(err.Error(), "Conflict") && !strings.Contains(err.Error(), "409") {
				return fmt.Errorf("failed to create topic %s: %w", topicNameStr, err)
			}
			slog.Info("Topic already exists", slog.String("topic", topicNameStr))
		} else {
			slog.Info("Created topic successfully", slog.String("topic", topicNameStr))
		}

		if err := p.configureTopic(*topicName, topicConfig); err != nil {
			return fmt.Errorf("failed to configure topic %s: %w", topicNameStr, err)
		}
	}

	return nil
}

func (p *PulsarInitializer) configureTopic(topicName utils.TopicName, config TopicConfig) error {
	if config.Retention != nil {
		slog.Info("Setting retention policy", slog.String("topic", topicName.String()))
		retentionData := utils.NewRetentionPolicies(config.Retention.TimeInMinutes, config.Retention.SizeInMB)

		err := p.adminClient.Topics().SetRetention(topicName, retentionData)
		if err != nil {
			slog.Warn("Failed to set retention policy", slog.String("topic", topicName.String()), slog.Any("err", err))
		} else {
			slog.Info("Retention policy set successfully", slog.String("topic", topicName.String()))
		}
	}

	return nil
}

func (p *PulsarInitializer) createSubscriptions(subscriptions []SubscriptionConfig) error {
	const defaultTenant = "public"

	for _, subConfig := range subscriptions {
		topicNameStr := fmt.Sprintf("persistent://%s/%s/%s", defaultTenant, p.config.Namespace, subConfig.Topic)
		subscriptionName := subConfig.Name

		slog.Info("Subscription will be created when first consumer connects",
			slog.String("topic", topicNameStr),
			slog.String("subscription", subscriptionName))
	}

	return nil
}
