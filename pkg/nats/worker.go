package nats

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

type MessageProcessor func(context.Context, jetstream.Msg) error

type WorkerConfig struct {
	StreamName        string
	ConsumerName      string
	DurableName       string
	Concurrency       int
	ProcessingTimeout time.Duration
	MaxDeliver        int
	AckWait           time.Duration
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
	healthy    atomic.Bool
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
	w.healthy.Store(true)

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
		slog.Error("failed to start message iterator",
			slog.String("stream", w.config.StreamName),
			slog.String("consumer", w.config.ConsumerName),
			slog.Any("error", err))
		w.healthy.Store(false)
		return
	}
	defer msgs.Stop()

	consecutiveErrors := 0
	const maxConsecutiveErrors = 10
	const errorBackoff = 500 * time.Millisecond

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
				consecutiveErrors++
				slog.Warn("failed to get next message",
					slog.String("stream", w.config.StreamName),
					slog.String("consumer", w.config.ConsumerName),
					slog.Int("consecutive_errors", consecutiveErrors),
					slog.Any("error", err))
				if consecutiveErrors >= maxConsecutiveErrors {
					slog.Error("too many consecutive message errors, stopping worker goroutine",
						slog.String("stream", w.config.StreamName),
						slog.String("consumer", w.config.ConsumerName))
					w.healthy.Store(false)
					return
				}
				time.Sleep(errorBackoff)
				continue
			}
			consecutiveErrors = 0

			procCtx, cancel := context.WithTimeout(ctx, w.config.ProcessingTimeout)
			err = w.processor(procCtx, msg)
			cancel()

			if err != nil {
				if nakErr := msg.Nak(); nakErr != nil {
					slog.Error("failed to nak message",
						slog.String("stream", w.config.StreamName),
						slog.Any("error", nakErr))
				}
			} else {
				if ackErr := msg.Ack(); ackErr != nil {
					slog.Error("failed to ack message",
						slog.String("stream", w.config.StreamName),
						slog.Any("error", ackErr))
				}
			}
		}
	}
}

func (w *natsWorker) HealthCheck() (bool, error) {
	if !w.healthy.Load() {
		return false, fmt.Errorf("worker %s/%s is unhealthy", w.config.StreamName, w.config.ConsumerName)
	}
	return true, nil
}
