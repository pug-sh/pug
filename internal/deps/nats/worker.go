package nats

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// PermanentError wraps errors that should not be retried. When a worker
// receives a PermanentError, the message is published to the configured DLQ
// subject and then terminated (not nacked for redelivery). Use for unrecoverable
// failures such as corrupt protobuf data. Use NewPermanentError to construct.
type PermanentError struct {
	err error
}

// NewPermanentError wraps err as a PermanentError. Panics if err is nil.
func NewPermanentError(err error) *PermanentError {
	if err == nil {
		panic("nats: NewPermanentError called with nil error")
	}
	return &PermanentError{err: err}
}

func (e *PermanentError) Error() string { return e.err.Error() }
func (e *PermanentError) Unwrap() error { return e.err }

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
	config    WorkerConfig
	processor MessageProcessor
	consumer  jetstream.Consumer
	js        jetstream.JetStream
	wg        sync.WaitGroup
	healthy   atomic.Bool
	started   atomic.Bool
}

func NewWorker(config WorkerConfig, processor MessageProcessor, client *NATSClient) (Worker, error) {
	if config.StreamName == "" {
		return nil, fmt.Errorf("nats: WorkerConfig.StreamName is required")
	}
	if config.ConsumerName == "" {
		return nil, fmt.Errorf("nats: WorkerConfig.ConsumerName is required")
	}
	if config.DurableName == "" {
		return nil, fmt.Errorf("nats: WorkerConfig.DurableName is required")
	}
	if processor == nil {
		return nil, fmt.Errorf("nats: processor must not be nil")
	}
	if config.DLQSubject == "" {
		return nil, fmt.Errorf("nats: WorkerConfig.DLQSubject is required")
	}
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
		config:    config,
		processor: processor,
		js:        client.GetJetStream(),
	}, nil
}

var errWorkerAlreadyStarted = errors.New("nats: worker already started")

func (w *natsWorker) Start(ctx context.Context) error {
	if !w.started.CompareAndSwap(false, true) {
		return errWorkerAlreadyStarted
	}

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

	<-ctx.Done()

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
		default:
		}

		slog.WarnContext(ctx, "restarting message processor after failure",
			slog.String("stream", w.config.StreamName),
			slog.String("consumer", w.config.ConsumerName))

		select {
		case <-ctx.Done():
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
			case IsPermanentError(err):
				slog.ErrorContext(ctx, "terminating poison message",
					slog.String("stream", w.config.StreamName),
					slog.String("consumer", w.config.ConsumerName),
					slog.Any("error", err))
				if !w.publishToDLQ(ctx, msg, err) {
					slog.ErrorContext(ctx, "DLQ publish failed for permanent error, terminating to avoid wasting retries",
						slog.String("stream", w.config.StreamName),
						slog.String("consumer", w.config.ConsumerName),
						slog.String("subject", msg.Subject()))
				}
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

				if w.isLastDelivery(ctx, msg) {
					if !w.publishToDLQ(ctx, msg, err) {
						slog.ErrorContext(ctx, "DLQ publish failed on last delivery, terminating to avoid silent message loss",
							slog.String("stream", w.config.StreamName),
							slog.String("consumer", w.config.ConsumerName),
							slog.String("subject", msg.Subject()))
					}
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

func (w *natsWorker) isLastDelivery(ctx context.Context, msg jetstream.Msg) bool {
	meta, err := msg.Metadata()
	if err != nil {
		slog.ErrorContext(ctx, "failed to read message metadata, treating as last delivery to preserve DLQ routing",
			slog.String("stream", w.config.StreamName),
			slog.String("consumer", w.config.ConsumerName),
			slog.Any("error", err))
		return true
	}
	return int(meta.NumDelivered) >= w.config.MaxDeliver
}

func (w *natsWorker) publishToDLQ(ctx context.Context, msg jetstream.Msg, processingErr error) bool {
	if w.config.DLQSubject == "" {
		slog.ErrorContext(ctx, "no DLQ subject configured, message will be redelivered",
			slog.String("stream", w.config.StreamName),
			slog.String("consumer", w.config.ConsumerName))
		return false
	}

	dlqMsg := &nats.Msg{
		Subject: w.config.DLQSubject,
		Data:    msg.Data(),
		Header:  nats.Header{},
	}
	// Copy original message headers for tracing and debugging.
	for k, v := range msg.Headers() {
		dlqMsg.Header[k] = v
	}
	dlqMsg.Header.Set("Original-Subject", msg.Subject())
	dlqMsg.Header.Set("Original-Stream", w.config.StreamName)
	dlqMsg.Header.Set("Original-Consumer", w.config.ConsumerName)
	if processingErr != nil {
		dlqMsg.Header.Set("Error-Reason", processingErr.Error())
	}
	dlqMsg.Header.Set("DLQ-Timestamp", time.Now().UTC().Format(time.RFC3339))
	if meta, err := msg.Metadata(); err == nil {
		dlqMsg.Header.Set("Delivery-Count", fmt.Sprintf("%d", meta.NumDelivered))
		dlqMsg.Header.Set("Stream-Sequence", fmt.Sprintf("%d", meta.Sequence.Stream))
	}

	if _, err := w.js.PublishMsg(ctx, dlqMsg); err != nil {
		slog.ErrorContext(ctx, "failed to publish message to DLQ",
			slog.String("stream", w.config.StreamName),
			slog.String("dlq_subject", w.config.DLQSubject),
			slog.Any("error", err))
		return false
	}

	slog.WarnContext(ctx, "message sent to DLQ",
		slog.String("stream", w.config.StreamName),
		slog.String("subject", msg.Subject()),
		slog.String("dlq_subject", w.config.DLQSubject))
	return true
}

func (w *natsWorker) HealthCheck() (bool, error) {
	if !w.healthy.Load() {
		return false, fmt.Errorf("worker %s/%s is unhealthy", w.config.StreamName, w.config.ConsumerName)
	}
	return true, nil
}
