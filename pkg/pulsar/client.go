package pulsar

import (
	"context"
	"fmt"
	"time"

	"github.com/apache/pulsar-client-go/pulsar"
	"github.com/sethvargo/go-envconfig"
)

// Config holds the Pulsar client configuration
type Config struct {
	URL      string `env:"PULSAR_URL,default=pulsar://localhost:6650"`
	Token    string `env:"PULSAR_TOKEN"`
	Tenant   string `env:"PULSAR_TENANT,default=public"`
	Namespace string `env:"PULSAR_NAMESPACE,default=default"`
}

// Client wraps the Pulsar client functionality
type Client struct {
	client pulsar.Client
	config *Config
}

// Producer wraps the Pulsar producer functionality
type Producer struct {
	producer pulsar.Producer
	topic    string
}

// Consumer wraps the Pulsar consumer functionality
type Consumer struct {
	consumer pulsar.Consumer
	topic    string
}

// NewClient creates a new Pulsar client
func NewClient(ctx context.Context) (*Client, error) {
	var cfg Config
	if err := envconfig.Process(ctx, &cfg); err != nil {
		return nil, fmt.Errorf("failed to process Pulsar config: %w", err)
	}

	client, err := pulsar.NewClient(pulsar.ClientOptions{
		URL:               cfg.URL,
		Authentication:    getAuthentication(cfg.Token),
		OperationTimeout:  30 * time.Second,
		ConnectionTimeout: 30 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create Pulsar client: %w", err)
	}

	return &Client{
		client: client,
		config: &cfg,
	}, nil
}

// getAuthentication creates authentication based on token
func getAuthentication(token string) pulsar.Authentication {
	if token != "" {
		return pulsar.NewAuthenticationToken(token)
	}
	return nil
}

// Close closes the Pulsar client
func (c *Client) Close() {
	if c.client != nil {
		c.client.Close()
	}
}

// CreateProducer creates a new Pulsar producer for the specified topic
func (c *Client) CreateProducer(topicName string) (*Producer, error) {
	fullTopicName := fmt.Sprintf("persistent://%s/%s/%s", c.config.Tenant, c.config.Namespace, topicName)
	
	producer, err := c.client.CreateProducer(pulsar.ProducerOptions{
		Topic: fullTopicName,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create producer for topic %s: %w", fullTopicName, err)
	}

	return &Producer{
		producer: producer,
		topic:    fullTopicName,
	}, nil
}

// CreateConsumer creates a new Pulsar consumer for the specified topic and subscription
func (c *Client) CreateConsumer(topicName, subscriptionName string, opts ...ConsumerOption) (*Consumer, error) {
	fullTopicName := fmt.Sprintf("persistent://%s/%s/%s", c.config.Tenant, c.config.Namespace, topicName)
	
	options := &ConsumerOptions{
		Type: pulsar.Shared,
	}
	for _, opt := range opts {
		opt(options)
	}
	
	consumer, err := c.client.Subscribe(pulsar.ConsumerOptions{
		Topic:            fullTopicName,
		SubscriptionName: subscriptionName,
		Type:             options.Type,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create consumer for topic %s with subscription %s: %w", 
			fullTopicName, subscriptionName, err)
	}

	return &Consumer{
		consumer: consumer,
		topic:    fullTopicName,
	}, nil
}

// Send sends a message to the producer topic
func (p *Producer) Send(ctx context.Context, msg *pulsar.ProducerMessage) error {
	_, err := p.producer.Send(ctx, msg)
	return err
}

// SendAsync sends a message asynchronously to the producer topic
func (p *Producer) SendAsync(ctx context.Context, msg *pulsar.ProducerMessage, callback func(pulsar.MessageID, *pulsar.ProducerMessage, error)) {
	p.producer.SendAsync(ctx, msg, callback)
}

// Chan returns the consumer's message channel
func (c *Consumer) Chan() <-chan pulsar.ConsumerMessage {
	return c.consumer.Chan()
}

// Ack acknowledges a message
func (c *Consumer) Ack(msg pulsar.Message) {
	c.consumer.Ack(msg)
}

// AckID acknowledges a message by ID
func (c *Consumer) AckID(msgID pulsar.MessageID) {
	c.consumer.AckID(msgID)
}

// Close closes the consumer
func (c *Consumer) Close() {
	if c.consumer != nil {
		c.consumer.Close()
	}
}

// ConsumerOptions holds options for creating a consumer
type ConsumerOptions struct {
	Type pulsar.SubscriptionType
}

// ConsumerOption is a function that configures consumer options
type ConsumerOption func(*ConsumerOptions)

// WithType sets the consumer type
func WithType(consumerType pulsar.SubscriptionType) ConsumerOption {
	return func(opts *ConsumerOptions) {
		opts.Type = consumerType
	}
}