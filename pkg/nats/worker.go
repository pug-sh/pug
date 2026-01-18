package nats

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

type MessageProcessor func(context.Context, jetstream.Msg) error

type WorkerConfig struct {
	StreamName          string
	ConsumerName        string
	DurableName         string
	Concurrency         int
	ProcessingTimeout   time.Duration
	MaxDeliver          int
	AckWait             time.Duration
}

type Worker interface {
	Start(ctx context.Context, client *NATSClient) error
	HealthCheck() (bool, error)
}

const (
	DefaultConcurrency       = 1
	DefaultProcessingTimeout = 30 * time.Second
	DefaultMaxDeliver        = 3
	DefaultAckWait           = 30 * time.Second
)

type natsWorker struct {
	config     WorkerConfig
	processor  MessageProcessor
	consumer   jetstream.Consumer
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
	if config.MaxDeliver <= 0 {
		config.MaxDeliver = DefaultMaxDeliver
	}
	if config.AckWait <= 0 {
		config.AckWait = DefaultAckWait
	}

	return &natsWorker{
		config:     config,
		processor:  processor,
		shutdownCh: make(chan struct{}),
	}, nil
}

func (w *natsWorker) Start(ctx context.Context, client *NATSClient) error {
	js := client.GetJetStream()

	consumer, err := js.CreateOrUpdateConsumer(ctx, w.config.StreamName, jetstream.ConsumerConfig{
		Name:          w.config.ConsumerName,
		Durable:       w.config.DurableName,
		AckPolicy:     jetstream.AckExplicitPolicy,
		MaxDeliver:    w.config.MaxDeliver,
		AckWait:       w.config.AckWait,
		DeliverPolicy: jetstream.DeliverAllPolicy,
		ReplayPolicy:  jetstream.ReplayInstantPolicy,
	})
	if err != nil {
		return fmt.Errorf("failed to create NATS consumer: %w", err)
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

func (w *natsWorker) processMessages(ctx context.Context) {
	defer w.wg.Done()

	msgs, err := w.consumer.Messages()
	if err != nil {
		return
	}
	defer msgs.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-w.shutdownCh:
			return
		default:
			msg, err := msgs.Next()
			if err != nil {
				if err == jetstream.ErrMsgIteratorClosed {
					return
				}
				continue
			}

			procCtx, cancel := context.WithTimeout(ctx, w.config.ProcessingTimeout)
			err = w.processor(procCtx, msg)
			cancel()

			if err != nil {
				msg.Nak()
			} else {
				msg.Ack()
			}
		}
	}
}

func (w *natsWorker) HealthCheck() (bool, error) {
	return true, nil
}