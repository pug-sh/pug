package nats

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/slogx"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// PermanentError wraps errors that should not be retried. When a worker
// receives a PermanentError, the message is published to the configured DLQ
// subject and then terminated (not nacked for redelivery). Use for unrecoverable
// failures such as corrupt protobuf data. Use NewPermanentError to construct.
//
// Attach structured context via With() — these key-value pairs are written as
// individual DLQ message headers for filtering and inspection without
// deserializing the payload.
type PermanentError struct {
	err      error
	metadata map[string]string
}

// NewPermanentError wraps err as a PermanentError. Panics if err is nil.
func NewPermanentError(err error) *PermanentError {
	if err == nil {
		panic("nats: NewPermanentError called with nil error")
	}
	return &PermanentError{err: err}
}

// With attaches structured key-value metadata to the error. These are written
// as headers on the DLQ message. Use lowercase snake_case keys.
func (e *PermanentError) With(key, value string) *PermanentError {
	if e.metadata == nil {
		e.metadata = make(map[string]string)
	}
	e.metadata[key] = value
	return e
}

// Metadata returns the structured context attached via With().
func (e *PermanentError) Metadata() map[string]string { return e.metadata }

func (e *PermanentError) Error() string { return e.err.Error() }
func (e *PermanentError) Unwrap() error { return e.err }

func IsPermanentError(err error) bool {
	_, ok := errors.AsType[*PermanentError](err)
	return ok
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
	// BackOff is the redelivery-delay schedule applied to retryable failures
	// via NakWithDelay: the delay before the i-th redelivery is BackOff[i-1],
	// with the last entry reused for any further attempts. This is applied
	// worker-side rather than via the consumer's BackOff field because the
	// consumer field overrides AckWait (which would let the server redeliver
	// mid-processing) and is bypassed for nack'ed messages — and this worker
	// always Naks explicitly. An empty schedule means immediate redelivery.
	BackOff    []time.Duration
	DLQSubject string
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

// DefaultBackOff is the redelivery-delay schedule used when WorkerConfig.BackOff
// is unset. It spaces retries out so a transient downstream outage (e.g. a brief
// ClickHouse or Postgres blip) is ridden out across attempts instead of burning
// every delivery in a tight loop and dead-lettering recoverable messages. Treated
// as read-only — do not mutate.
var DefaultBackOff = []time.Duration{
	1 * time.Second,
	5 * time.Second,
	15 * time.Second,
	30 * time.Second,
}

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
	if len(config.BackOff) == 0 {
		// Clone so each worker owns its slice — a later append on one worker's
		// schedule must never write into the shared DefaultBackOff backing array.
		config.BackOff = slices.Clone(DefaultBackOff)
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

	// Expose this worker on the process-wide /healthz endpoint so an
	// orchestrator can liveness-probe it and restart a wedged worker.
	registerHealth(ctx, w)

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
			slogx.Error(err))
		w.healthy.Store(false)
		return
	}
	defer msgs.Stop()

	// Unblock msgs.Next() when the context is cancelled. The goroutine is
	// scoped to runMessageLoop via loopCancel so it does not leak on restart.
	loopCtx, loopCancel := context.WithCancel(ctx)
	defer loopCancel()

	go func() {
		<-loopCtx.Done()
		msgs.Stop()
	}()

	w.healthy.Store(true)

	consecutiveErrors := 0
	const maxConsecutiveErrors = 10
	const errorBackoff = 500 * time.Millisecond

	for {
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
				slogx.Error(err))
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

		w.handleMessage(ctx, msg)
	}
}

// handleMessage processes a single message with OTel span tracking. Extracted
// from the message loop so that defer correctly scopes cancel() and span.End()
// to each message, preventing resource leaks on panics.
func (w *natsWorker) handleMessage(ctx context.Context, msg jetstream.Msg) {
	procCtx, cancel := context.WithTimeout(ctx, w.config.ProcessingTimeout)
	defer cancel()

	procCtx = extractTraceContext(procCtx, msg)

	// Read metadata once: it feeds both the consumer-span attributes and the
	// last-delivery routing decision below, so a single snapshot keeps the
	// backoff and DLQ decisions consistent. On failure we record once here and
	// fall back to treating the delivery as last (see isLastDelivery).
	meta, metaErr := msg.Metadata()
	metaOK := metaErr == nil
	if !metaOK {
		slog.WarnContext(procCtx, "failed to read message metadata",
			slog.String("stream", w.config.StreamName),
			slog.String("consumer", w.config.ConsumerName),
			slogx.Error(metaErr))
		telemetry.RecordError(procCtx, metaErr)
	}
	var streamSeq, consumerSeq uint64
	var numDelivered uint64
	if meta != nil {
		numDelivered = meta.NumDelivered
		streamSeq = meta.Sequence.Stream
		consumerSeq = meta.Sequence.Consumer
	}
	procCtx, span := startConsumerSpan(procCtx, msg.Subject(), w.config.StreamName, w.config.ConsumerName, numDelivered, streamSeq, consumerSeq)
	defer span.End()

	// processor logs+records its own errors per the log-at-source convention.
	// The wrapper logs the disposition (term/nak/ack/DLQ) it decided on plus
	// any wrapper-detected secondary failures. The disposition log includes
	// slogx.Error(err) as annotation — it is a different log line than the
	// processor's source log (different message, different fact), so attaching
	// the cause is not "re-logging the same error" per the convention.
	switch err := w.processor(procCtx, msg); {
	case IsPermanentError(err):
		slog.ErrorContext(procCtx, "terminating poison message",
			slog.String("stream", w.config.StreamName),
			slog.String("consumer", w.config.ConsumerName),
			slogx.Error(err))
		dlqCtx, dlqCancel := dlqContext(procCtx)
		published := w.publishToDLQ(dlqCtx, msg, err)
		dlqCancel()
		recordDLQOutcome(procCtx, w.config.StreamName, w.config.ConsumerName, dispositionPermanent, published)
		if !published {
			slog.ErrorContext(procCtx, "DLQ publish failed for permanent error, terminating to avoid wasting retries",
				slog.String("stream", w.config.StreamName),
				slog.String("consumer", w.config.ConsumerName),
				slog.String("subject", msg.Subject()))
		}
		if termErr := msg.Term(); termErr != nil {
			slog.ErrorContext(procCtx, "failed to terminate message",
				slog.String("stream", w.config.StreamName),
				slogx.Error(termErr))
			telemetry.RecordError(procCtx, termErr)
		}
	case err != nil:
		slog.ErrorContext(procCtx, "message processing failed",
			slog.String("stream", w.config.StreamName),
			slog.String("consumer", w.config.ConsumerName),
			slogx.Error(err))

		if w.isLastDelivery(numDelivered, metaOK) {
			dlqCtx, dlqCancel := dlqContext(procCtx)
			published := w.publishToDLQ(dlqCtx, msg, err)
			dlqCancel()
			recordDLQOutcome(procCtx, w.config.StreamName, w.config.ConsumerName, dispositionExhausted, published)
			if !published {
				slog.ErrorContext(procCtx, "DLQ publish failed on last delivery, terminating to avoid silent message loss",
					slog.String("stream", w.config.StreamName),
					slog.String("consumer", w.config.ConsumerName),
					slog.String("subject", msg.Subject()))
			}
			if termErr := msg.Term(); termErr != nil {
				slog.ErrorContext(procCtx, "failed to term message",
					slog.String("stream", w.config.StreamName),
					slogx.Error(termErr))
				telemetry.RecordError(procCtx, termErr)
			}
		} else {
			delay := w.nakBackoff(numDelivered)
			if nakErr := msg.NakWithDelay(delay); nakErr != nil {
				slog.ErrorContext(procCtx, "failed to nak message",
					slog.String("stream", w.config.StreamName),
					slog.Duration("redelivery_delay", delay),
					slogx.Error(nakErr))
				telemetry.RecordError(procCtx, nakErr)
			}
		}
	default:
		if ackErr := msg.Ack(); ackErr != nil {
			slog.ErrorContext(procCtx, "failed to ack message",
				slog.String("stream", w.config.StreamName),
				slogx.Error(ackErr))
			telemetry.RecordError(procCtx, ackErr)
		}
	}
}

// nakBackoff returns the redelivery delay for a message whose processing failed
// on its numDelivered-th attempt, following the configured BackOff schedule. The
// last interval is reused for any attempts beyond the schedule's length. Returns
// 0 (immediate redelivery) when no schedule is configured. A zero numDelivered
// (metadata unavailable) is clamped to the first interval.
func (w *natsWorker) nakBackoff(numDelivered uint64) time.Duration {
	if len(w.config.BackOff) == 0 {
		return 0
	}
	idx := max(int(numDelivered)-1, 0)
	idx = min(idx, len(w.config.BackOff)-1)
	return w.config.BackOff[idx]
}

// isLastDelivery reports whether this is the final delivery attempt before the
// message must be dead-lettered. numDelivered and metaOK come from the single
// Metadata() read in handleMessage, so the routing decision shares one snapshot
// with the backoff decision. When metadata was unavailable (metaOK false) we
// treat the delivery as last to preserve DLQ routing rather than risk an endless
// redelivery loop.
func (w *natsWorker) isLastDelivery(numDelivered uint64, metaOK bool) bool {
	if !metaOK {
		return true
	}
	return int(numDelivered) >= w.config.MaxDeliver
}

// dlqPublishTimeout bounds the DLQ publish on the deadline-free context below.
const dlqPublishTimeout = 5 * time.Second

// DLQ disposition labels for the nats.dlq_messages_total metric.
const (
	dispositionPermanent = "permanent"   // poison message, never retried
	dispositionExhausted = "max_deliver" // retries exhausted
)

// dlqMessageCounter counts every attempt to route a message to a DLQ.
// outcome="published" is a dead-letter that safely landed; outcome="dropped" is
// a message LOST because the DLQ publish itself failed (it is Term'd afterward).
// Operators should alert on any outcome="dropped" — it is unrecoverable data loss.
var dlqMessageCounter metric.Int64Counter

func init() {
	meter := otel.Meter("github.com/pug-sh/pug/internal/deps/nats")
	// Panic on init failure: without this counter the only signal for dropped
	// dead-letters is a log line, and silent DLQ loss is exactly what it guards.
	c, err := meter.Int64Counter(
		"nats.dlq_messages_total",
		metric.WithDescription("Messages routed to a DLQ; outcome=dropped means the DLQ publish failed and the message was lost."),
	)
	if err != nil {
		panic("nats: failed to register nats.dlq_messages_total counter: " + err.Error())
	}
	dlqMessageCounter = c
}

// recordDLQOutcome emits the nats.dlq_messages_total metric for one dead-letter
// attempt, tagged by stream, consumer, disposition, and whether it landed.
func recordDLQOutcome(ctx context.Context, stream, consumer, disposition string, published bool) {
	outcome := "published"
	if !published {
		outcome = "dropped"
	}
	dlqMessageCounter.Add(ctx, 1, metric.WithAttributes(
		attribute.String("stream", stream),
		attribute.String("consumer", consumer),
		attribute.String("disposition", disposition),
		attribute.String("outcome", outcome),
	))
}

// dlqContext returns a context for the DLQ publish that is decoupled from the
// per-message processing deadline. A message that fails *because* it exhausted
// its ProcessingTimeout arrives with procCtx already cancelled; publishing the
// DLQ copy on that dead context would fail and force a Term() that silently
// drops the very message we most want to capture. WithoutCancel keeps the trace
// context (so the DLQ producer span links to the consumer span) while shedding
// the expired deadline; a fresh short timeout bounds the publish.
func dlqContext(procCtx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(procCtx), dlqPublishTimeout)
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
	maps.Copy(dlqMsg.Header, msg.Headers())
	dlqMsg.Header.Set("original_subject", msg.Subject())
	dlqMsg.Header.Set("original_stream", w.config.StreamName)
	dlqMsg.Header.Set("original_consumer", w.config.ConsumerName)
	if processingErr != nil {
		dlqMsg.Header.Set("error_reason", processingErr.Error())
		if pe, ok := errors.AsType[*PermanentError](processingErr); ok {
			for k, v := range pe.Metadata() {
				dlqMsg.Header.Set(k, v)
			}
		}
	}
	dlqMsg.Header.Set("dlq_timestamp", time.Now().UTC().Format(time.RFC3339))
	if meta, err := msg.Metadata(); err == nil {
		dlqMsg.Header.Set("delivery_count", fmt.Sprintf("%d", meta.NumDelivered))
		dlqMsg.Header.Set("stream_sequence", fmt.Sprintf("%d", meta.Sequence.Stream))
	} else {
		// Warn (not Error) — metadata is best-effort for DLQ debugging headers, the
		// message still gets DLQ'd. RecordError surfaces systemic failures on the span
		// without escalating individual events.
		slog.WarnContext(ctx, "failed to read message metadata for DLQ headers",
			slog.String("stream", w.config.StreamName),
			slog.String("dlq_subject", w.config.DLQSubject),
			slogx.Error(err))
		telemetry.RecordError(ctx, err)
	}

	if _, err := w.js.PublishMsg(ctx, dlqMsg); err != nil {
		slog.ErrorContext(ctx, "failed to publish message to DLQ",
			slog.String("stream", w.config.StreamName),
			slog.String("dlq_subject", w.config.DLQSubject),
			slogx.Error(err))
		telemetry.RecordError(ctx, err)
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
