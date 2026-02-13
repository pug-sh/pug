package nats

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// ErrMessageHandled signals that the processor already acknowledged the message
// (e.g. via Term). The worker skips both Ack and Nak.
var ErrMessageHandled = errors.New("message already handled")

// PermanentError wraps errors that should not be retried. When a worker
// receives a PermanentError, it terminates the NATS message instead of
// nacking it for redelivery (e.g. corrupt protobuf data).
type PermanentError struct {
	Err error
}

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

func IsPermanentError(err error) bool {
	var pe *PermanentError
	return errors.As(err, &pe)
}

type MessageProcessor func(context.Context, jetstream.Msg) error

type WorkerConfig struct {
	StreamName        string
	ConsumerName      string
	DurableName       string
	FilterSubject     string
	Concurrency       int
	ProcessingTimeout time.Duration
	MaxDeliver        int
	AckWait           time.Duration
	DLQSubject        string
}

type Worker interface {
	Start(ctx context.Context) error
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
	js         jetstream.JetStream
	shutdownCh chan struct{}
	wg         sync.WaitGroup
	healthy    atomic.Bool
}

func NewWorker(config WorkerConfig, processor MessageProcessor, client *NATSClient) (Worker, error) {
	if config.Concurrency <= 0 {
		config.Concurrency = DefaultConcurrency
	}
	if config.ProcessingTimeout <= 0 {
		config.ProcessingTimeout = DefaultProcessingTimeout
	}
	// Safety net: ensure a finite retry limit even if YAML config omits max_deliver
	if config.MaxDeliver <= 0 {
		config.MaxDeliver = DefaultMaxDeliver
	}
	if config.AckWait <= 0 {
		config.AckWait = DefaultAckWait
	}

	return &natsWorker{
		config:     config,
		processor:  processor,
		js:         client.GetJetStream(),
		shutdownCh: make(chan struct{}),
	}, nil
}

func (w *natsWorker) Start(ctx context.Context) error {
	consumerConfig := jetstream.ConsumerConfig{
		Name:          w.config.ConsumerName,
		Durable:       w.config.DurableName,
		AckPolicy:     jetstream.AckExplicitPolicy,
		MaxDeliver:    w.config.MaxDeliver,
		AckWait:       w.config.AckWait,
		DeliverPolicy: jetstream.DeliverAllPolicy,
		ReplayPolicy:  jetstream.ReplayInstantPolicy,
	}

	if w.config.FilterSubject != "" {
		consumerConfig.FilterSubject = w.config.FilterSubject
	}

	consumer, err := w.js.CreateOrUpdateConsumer(ctx, w.config.StreamName, consumerConfig)
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

	const restartBackoff = 5 * time.Second

	for {
		w.runMessageLoop(ctx)

		// Check if we should restart or exit permanently.
		select {
		case <-ctx.Done():
			return
		case <-w.shutdownCh:
			return
		default:
		}

		slog.WarnContext(ctx, "restarting message processor after failure",
			slog.String("stream", w.config.StreamName),
			slog.String("consumer", w.config.ConsumerName))

		select {
		case <-ctx.Done():
			return
		case <-w.shutdownCh:
			return
		case <-time.After(restartBackoff):
		}
	}
}

func (w *natsWorker) runMessageLoop(ctx context.Context) {
	msgs, err := w.consumer.Messages()
	if err != nil {
		slog.ErrorContext(ctx, "failed to start message iterator",
			slog.String("stream", w.config.StreamName),
			slog.String("consumer", w.config.ConsumerName),
			slog.Any("error", err))
		w.healthy.Store(false)
		return
	}
	defer msgs.Stop()
	w.healthy.Store(true)

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
				slog.WarnContext(ctx, "failed to get next message",
					slog.String("stream", w.config.StreamName),
					slog.String("consumer", w.config.ConsumerName),
					slog.Int("consecutive_errors", consecutiveErrors),
					slog.Any("error", err))
				if consecutiveErrors >= maxConsecutiveErrors {
					slog.ErrorContext(ctx, "too many consecutive message errors, restarting worker goroutine",
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

			switch {
			case errors.Is(err, ErrMessageHandled):
				// Processor already handled acknowledgment (e.g. via Term).
			case IsPermanentError(err):
				slog.ErrorContext(ctx, "terminating poison message",
					slog.String("stream", w.config.StreamName),
					slog.String("consumer", w.config.ConsumerName),
					slog.Any("error", err))
				if termErr := msg.Term(); termErr != nil {
					slog.ErrorContext(ctx, "failed to terminate message",
						slog.String("stream", w.config.StreamName),
						slog.Any("error", termErr))
				}
			case err != nil:
				slog.ErrorContext(ctx, "message processing failed",
					slog.String("stream", w.config.StreamName),
					slog.String("consumer", w.config.ConsumerName),
					slog.Any("error", err))

				if w.isLastDelivery(msg) {
					w.publishToDLQ(ctx, msg)
					if termErr := msg.Term(); termErr != nil {
						slog.ErrorContext(ctx, "failed to term message",
							slog.String("stream", w.config.StreamName),
							slog.Any("error", termErr))
					}
				} else {
					if nakErr := msg.Nak(); nakErr != nil {
						slog.ErrorContext(ctx, "failed to nak message",
							slog.String("stream", w.config.StreamName),
							slog.Any("error", nakErr))
					}
				}
			default:
				if ackErr := msg.Ack(); ackErr != nil {
					slog.ErrorContext(ctx, "failed to ack message",
						slog.String("stream", w.config.StreamName),
						slog.Any("error", ackErr))
				}
			}
		}
	}
}

func (w *natsWorker) isLastDelivery(msg jetstream.Msg) bool {
	meta, err := msg.Metadata()
	if err != nil {
		return false
	}
	return int(meta.NumDelivered) >= w.config.MaxDeliver
}

func (w *natsWorker) publishToDLQ(ctx context.Context, msg jetstream.Msg) {
	if w.config.DLQSubject == "" || w.js == nil {
		return
	}

	if _, err := w.js.Publish(ctx, w.config.DLQSubject, msg.Data()); err != nil {
		slog.ErrorContext(ctx, "failed to publish message to DLQ",
			slog.String("stream", w.config.StreamName),
			slog.String("dlq_subject", w.config.DLQSubject),
			slog.Any("error", err))
	} else {
		slog.WarnContext(ctx, "message sent to DLQ",
			slog.String("stream", w.config.StreamName),
			slog.String("dlq_subject", w.config.DLQSubject))
	}
}

func (w *natsWorker) HealthCheck() (bool, error) {
	if !w.healthy.Load() {
		return false, fmt.Errorf("worker %s/%s is unhealthy", w.config.StreamName, w.config.ConsumerName)
	}
	return true, nil
}
