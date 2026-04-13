package nats

import (
	"context"
	"maps"
	"slices"

	"github.com/fivebitsio/cotton/internal/deps/telemetry"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const tracerName = "cotton/nats"

var (
	attrMessagingSystem      = attribute.Key("messaging.system")
	attrDestinationName      = attribute.Key("messaging.destination.name")
	attrBodySize             = attribute.Key("messaging.message.body.size")
	attrMessagingOp          = attribute.Key("messaging.operation")
	attrMessagingStream      = attribute.Key("messaging.destination.definition")
	attrMessagingConsumer    = attribute.Key("messaging.consumer.name")
	attrMessagingDeliveries  = attribute.Key("messaging.message.delivery_number")
	attrMessagingSeq         = attribute.Key("messaging.message.sequence")
	attrMessagingConsumerSeq = attribute.Key("messaging.consumer.sequence")
)

// headerCarrier adapts nats.Header to propagation.TextMapCarrier so that the
// standard OTel propagator can inject and extract W3C TraceContext headers
// (traceparent, tracestate) directly on NATS message headers.
type headerCarrier nats.Header

func (c headerCarrier) Get(key string) string {
	return nats.Header(c).Get(key)
}

func (c headerCarrier) Set(key, val string) {
	nats.Header(c).Set(key, val)
}

func (c headerCarrier) Keys() []string {
	return slices.Collect(maps.Keys(nats.Header(c)))
}

// tracedJetStream wraps jetstream.JetStream and injects OTel trace context into
// every outbound message. Only Publish and PublishMsg are overridden; all other
// methods (including PublishAsync/PublishMsgAsync) pass through untraced.
type tracedJetStream struct {
	jetstream.JetStream
}

// Publish delegates to the embedded JetStream.PublishMsg (not t.PublishMsg) because
// Publish(subject, data) provides no header access for W3C trace context injection.
// Calling the embedded PublishMsg directly avoids double-spanning.
func (t *tracedJetStream) Publish(ctx context.Context, subject string, data []byte, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	ctx, span := startProducerSpan(ctx, subject, len(data))
	defer span.End()

	msg := &nats.Msg{Subject: subject, Data: data, Header: make(nats.Header)}
	injectTraceContext(ctx, msg)

	ack, err := t.JetStream.PublishMsg(ctx, msg, opts...)
	if err != nil {
		telemetry.RecordError(ctx, err)
	}
	return ack, err
}

func (t *tracedJetStream) PublishMsg(ctx context.Context, msg *nats.Msg, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	if msg.Header == nil {
		msg.Header = make(nats.Header)
	}
	ctx, span := startProducerSpan(ctx, msg.Subject, len(msg.Data))
	defer span.End()

	injectTraceContext(ctx, msg)

	ack, err := t.JetStream.PublishMsg(ctx, msg, opts...)
	if err != nil {
		telemetry.RecordError(ctx, err)
	}
	return ack, err
}

// injectTraceContext writes the active span's W3C TraceContext headers into msg.
func injectTraceContext(ctx context.Context, msg *nats.Msg) {
	otel.GetTextMapPropagator().Inject(ctx, headerCarrier(msg.Header))
}

// extractTraceContext reads W3C TraceContext headers from msg and returns a
// context containing the restored remote span context. If no headers are
// present the original ctx is returned unchanged.
func extractTraceContext(ctx context.Context, msg jetstream.Msg) context.Context {
	return otel.GetTextMapPropagator().Extract(ctx, headerCarrier(msg.Headers()))
}

func startProducerSpan(ctx context.Context, subject string, payloadSize int) (context.Context, trace.Span) {
	return otel.Tracer(tracerName).Start(ctx, "send "+subject,
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(
			attrMessagingSystem.String("nats"),
			attrDestinationName.String(subject),
			attrBodySize.Int(payloadSize),
		),
	)
}

func startConsumerSpan(ctx context.Context, subject, stream, consumer string, numDelivered uint64, streamSeq, consumerSeq uint64) (context.Context, trace.Span) {
	return otel.Tracer(tracerName).Start(ctx, "process "+subject,
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attrMessagingSystem.String("nats"),
			attrDestinationName.String(subject),
			attrMessagingStream.String(stream),
			attrMessagingConsumer.String(consumer),
			attrMessagingDeliveries.Int64(int64(numDelivered)),
			attrMessagingSeq.Int64(int64(streamSeq)),
			attrMessagingConsumerSeq.Int64(int64(consumerSeq)),
			attrMessagingOp.String("process"),
		),
	)
}
