package pulsar

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/apache/pulsar-client-go/pulsar"
)

type MessageProcessor func(context.Context, pulsar.Message) error

type WorkerConfig struct {
	ClientOptions     pulsar.ClientOptions
	ConsumerOptions   pulsar.ConsumerOptions
	DeadLetterTopic   string
	Concurrency       int
	ProcessingTimeout time.Duration
}

type Worker interface {
	Start(ctx context.Context, client *Client) error
	HealthCheck() (bool, error)
}

const (
	DefaultConcurrency       = 1
	DefaultProcessingTimeout = 30 * time.Second
)

type pulsarWorker struct {
	config     WorkerConfig
	processor  MessageProcessor
	consumer   *Consumer
	shutdownCh chan struct{}
	wg         sync.WaitGroup
}

func NewWorker(config WorkerConfig, processor MessageProcessor) (Worker, error) {
	if config.Concurrency <= 0 {
		config.Concurrency = DefaultConcurrency
	}
	if config.ProcessingTimeout <= 0 {
		config.ProcessingTimeout = DefaultProcessingTimeout
	}

	return &pulsarWorker{
		config:     config,
		processor:  processor,
		shutdownCh: make(chan struct{}),
	}, nil
}

func (w *pulsarWorker) Start(ctx context.Context, client *Client) error {
	consumer, err := client.CreateConsumer(w.config.ConsumerOptions.Topic, w.config.ConsumerOptions.SubscriptionName, WithType(w.config.ConsumerOptions.Type))
	if err != nil {
		return fmt.Errorf("failed to create Pulsar consumer: %w", err)
	}
	w.consumer = consumer

	for i := 0; i < w.config.Concurrency; i++ {
		w.wg.Add(1)
		go w.processMessages(ctx)
	}

	select {
	case <-ctx.Done():
	case <-w.shutdownCh:
	}

	w.consumer.Close()

	done := make(chan struct{})
	go func() {
		w.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-time.After(5 * time.Second):
		return fmt.Errorf("timeout waiting for workers to finish")
	}
}

func (w *pulsarWorker) processMessages(ctx context.Context) {
	defer w.wg.Done()

	for {
		select {
		case <-ctx.Done():
			return
		case <-w.shutdownCh:
			return
		default:
			select {
			case msg, ok := <-w.consumer.Chan():
				if !ok {
					return
				}

				// Process the message with timeout
				procCtx, cancel := context.WithTimeout(ctx, w.config.ProcessingTimeout)
				err := w.processor(procCtx, msg.Message)
				cancel()

				if err != nil {
					// Negative acknowledge to trigger redelivery from broker
					w.consumer.Nack(msg.Message)
				} else {
					// Acknowledge on successful processing
					w.consumer.Ack(msg.Message)
				}
			case <-ctx.Done():
				return
			}
		}
	}
}

func (w *pulsarWorker) HealthCheck() (bool, error) {
	return true, nil
}
