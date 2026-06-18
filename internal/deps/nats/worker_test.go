package nats

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// fakeMsg implements jetstream.Msg for disposition tests. Only the methods
// handleMessage/publishToDLQ touch are overridden; the embedded nil interface
// satisfies the rest (never called). Ack/Nak/NakWithDelay/Term record calls so
// tests can assert which disposition handleMessage chose.
type fakeMsg struct {
	jetstream.Msg
	data    []byte
	subject string
	headers nats.Header
	meta    *jetstream.MsgMetadata
	metaErr error

	ackCalls  int
	nakCalls  int
	nakDelays []time.Duration
	termCalls int
}

func (m *fakeMsg) Data() []byte                              { return m.data }
func (m *fakeMsg) Subject() string                           { return m.subject }
func (m *fakeMsg) Headers() nats.Header                      { return m.headers }
func (m *fakeMsg) Metadata() (*jetstream.MsgMetadata, error) { return m.meta, m.metaErr }
func (m *fakeMsg) Ack() error                                { m.ackCalls++; return nil }
func (m *fakeMsg) Nak() error                                { m.nakCalls++; return nil }
func (m *fakeMsg) NakWithDelay(d time.Duration) error {
	m.nakCalls++
	m.nakDelays = append(m.nakDelays, d)
	return nil
}
func (m *fakeMsg) Term() error { m.termCalls++; return nil }

// fakeJetStream implements jetstream.JetStream for DLQ tests. publishToDLQ only
// calls PublishMsg, so the embedded nil interface covers everything else.
type fakeJetStream struct {
	jetstream.JetStream
	publishErr    error
	published     []*nats.Msg
	publishCtxErr error // set if PublishMsg was ever called with a dead context
}

func (f *fakeJetStream) PublishMsg(ctx context.Context, msg *nats.Msg, _ ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	// Context-aware so tests can observe whether the DLQ publish ran on a LIVE
	// context. A publish attempted on an already-cancelled context records nothing
	// in published and fails — exactly the failure that occurs if dlqContext stops
	// shedding the expired per-message processing deadline (the headline DLQ-loss
	// bug). Existing callers pass a live context, so their behavior is unchanged.
	if err := ctx.Err(); err != nil {
		f.publishCtxErr = err
		return nil, err
	}
	f.published = append(f.published, msg)
	if f.publishErr != nil {
		return nil, f.publishErr
	}
	return &jetstream.PubAck{}, nil
}

func newTestWorker(proc MessageProcessor, js jetstream.JetStream) *natsWorker {
	return &natsWorker{
		config: WorkerConfig{
			StreamName:        "s",
			ConsumerName:      "c",
			MaxDeliver:        3,
			ProcessingTimeout: 5 * time.Second,
			BackOff:           []time.Duration{1 * time.Second, 5 * time.Second, 15 * time.Second},
			DLQSubject:        "dlq.s",
		},
		processor: proc,
		js:        js,
	}
}

func failProc(context.Context, jetstream.Msg) error { return errors.New("boom") }

func TestNakBackoff(t *testing.T) {
	schedule := []time.Duration{1 * time.Second, 5 * time.Second, 15 * time.Second, 30 * time.Second}
	w := &natsWorker{config: WorkerConfig{BackOff: schedule}}

	tests := []struct {
		name         string
		numDelivered uint64
		want         time.Duration
	}{
		{"metadata unavailable clamps to first", 0, 1 * time.Second},
		{"first attempt", 1, 1 * time.Second},
		{"second attempt", 2, 5 * time.Second},
		{"third attempt", 3, 15 * time.Second},
		{"fourth attempt", 4, 30 * time.Second},
		{"beyond schedule reuses last", 9, 30 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := w.nakBackoff(tt.numDelivered); got != tt.want {
				t.Errorf("nakBackoff(%d) = %v, want %v", tt.numDelivered, got, tt.want)
			}
		})
	}
}

// An empty schedule means immediate redelivery (delay 0), matching the prior
// bare-Nak behavior so a worker that explicitly clears BackOff opts back out.
func TestNakBackoffEmptySchedule(t *testing.T) {
	w := &natsWorker{config: WorkerConfig{BackOff: nil}}
	for _, n := range []uint64{0, 1, 3, 100} {
		if got := w.nakBackoff(n); got != 0 {
			t.Errorf("nakBackoff(%d) with empty schedule = %v, want 0", n, got)
		}
	}
}

// NewWorker applies defaultBackOff() when the caller leaves it unset, so every
// worker gets spaced retries without per-worker wiring.
func TestNewWorkerDefaultsBackOff(t *testing.T) {
	w, err := NewWorker(WorkerConfig{
		StreamName:   "s",
		ConsumerName: "c",
		DurableName:  "d",
		DLQSubject:   "dlq.s",
	}, func(_ context.Context, _ jetstream.Msg) error { return nil }, &NATSClient{})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	nw := w.(*natsWorker)
	if len(nw.config.BackOff) != len(defaultBackOff()) {
		t.Fatalf("BackOff = %v, want default %v", nw.config.BackOff, defaultBackOff())
	}
}

// NewWorker validates a custom BackOff: a non-positive entry would hand
// NakWithDelay a zero/negative delay, silently defeating the backoff for that
// attempt, so it is rejected at construction (the same boundary that validates
// every other WorkerConfig field).
func TestNewWorkerRejectsNonPositiveBackOff(t *testing.T) {
	_, err := NewWorker(WorkerConfig{
		StreamName:   "s",
		ConsumerName: "c",
		DurableName:  "d",
		DLQSubject:   "dlq.s",
		BackOff:      []time.Duration{1 * time.Second, 0, 5 * time.Second},
	}, func(_ context.Context, _ jetstream.Msg) error { return nil }, &NATSClient{})
	if err == nil {
		t.Fatal("NewWorker accepted a BackOff with a non-positive entry, want error")
	}
}

func TestWorkerHealthCheck(t *testing.T) {
	w := &natsWorker{config: WorkerConfig{StreamName: "s", ConsumerName: "c"}}

	if ok, err := w.HealthCheck(); ok || err == nil {
		t.Fatalf("fresh worker: got (ok=%v, err=%v), want unhealthy with error", ok, err)
	}

	w.healthy.Store(true)
	if ok, err := w.HealthCheck(); !ok || err != nil {
		t.Fatalf("healthy worker: got (ok=%v, err=%v), want healthy with no error", ok, err)
	}

	w.healthy.Store(false)
	if ok, _ := w.HealthCheck(); ok {
		t.Fatal("unhealthy worker reported healthy")
	}
}

// Ready (readiness) requires BOTH that the worker has started consuming AND that
// its NATS connection is currently live — distinct from HealthCheck (liveness),
// which only tracks whether the consume loop is wedged. A disconnected worker is
// not ready (pull it from rotation) but not necessarily dead (don't restart it).
func TestWorkerReady(t *testing.T) {
	w := &natsWorker{
		config:    WorkerConfig{StreamName: "s", ConsumerName: "c"},
		connected: func() bool { return true },
	}

	if ok, err := w.Ready(); ok || err == nil {
		t.Fatalf("not started: got (ok=%v, err=%v), want not ready with error", ok, err)
	}

	w.started.Store(true)
	if ok, err := w.Ready(); !ok || err != nil {
		t.Fatalf("started + connected: got (ok=%v, err=%v), want ready with no error", ok, err)
	}

	w.connected = func() bool { return false }
	if ok, err := w.Ready(); ok || err == nil {
		t.Fatalf("started but NATS down: got (ok=%v, err=%v), want not ready with error", ok, err)
	}
}

// Ready must fail closed — not panic — when the connectivity probe is unwired
// (nil connected func). NewWorker always sets it, but the defensive nil guard in
// Ready has no other test pinning it, so a regression that drops the guard would
// otherwise surface only as a nil-func panic in production.
func TestWorkerReadyNilConnectedFailsClosed(t *testing.T) {
	w := &natsWorker{config: WorkerConfig{StreamName: "s", ConsumerName: "c"}} // connected left nil
	w.started.Store(true)

	if ok, err := w.Ready(); ok || err == nil {
		t.Fatalf("nil connected: got (ok=%v, err=%v), want not ready with error", ok, err)
	}
}

// The registry snapshots report the first failing worker — liveness via
// HealthCheck, readiness via Ready. A fresh healthRegistry per case needs no
// process-global save/restore, which is exactly the test-pollution hazard that
// boxing the registry into a value type removes.
func TestHealthSnapshot(t *testing.T) {
	okWorker := &natsWorker{config: WorkerConfig{StreamName: "a", ConsumerName: "a"}, connected: func() bool { return true }}
	okWorker.healthy.Store(true)
	okWorker.started.Store(true)

	// Liveness: all healthy → nil; one unhealthy → error.
	unhealthy := &natsWorker{config: WorkerConfig{StreamName: "b", ConsumerName: "b"}}
	if err := (&healthRegistry{workers: []Worker{okWorker}}).livenessSnapshot(); err != nil {
		t.Fatalf("liveness all healthy: got %v, want nil", err)
	}
	if err := (&healthRegistry{workers: []Worker{okWorker, unhealthy}}).livenessSnapshot(); err == nil {
		t.Fatal("liveness with one unhealthy worker: got nil, want error")
	}

	// Readiness: all ready → nil; one not-ready (NATS down) → error.
	notReady := &natsWorker{config: WorkerConfig{StreamName: "c", ConsumerName: "c"}, connected: func() bool { return false }}
	notReady.started.Store(true)
	if err := (&healthRegistry{workers: []Worker{okWorker}}).readinessSnapshot(); err != nil {
		t.Fatalf("readiness all ready: got %v, want nil", err)
	}
	if err := (&healthRegistry{workers: []Worker{okWorker, notReady}}).readinessSnapshot(); err == nil {
		t.Fatal("readiness with one not-ready worker: got nil, want error")
	}
}

// dlqContext must shed an already-expired processing deadline so the DLQ publish
// for a timed-out message is not dead on arrival.
func TestDLQContextShedsDeadline(t *testing.T) {
	procCtx, cancel := context.WithTimeout(context.Background(), -time.Second) // already expired
	defer cancel()
	if procCtx.Err() == nil {
		t.Fatal("precondition: expected procCtx to be expired")
	}

	dlqCtx, dlqCancel := dlqContext(procCtx)
	defer dlqCancel()

	if err := dlqCtx.Err(); err != nil {
		t.Fatalf("dlqContext is already done (err=%v); the expired deadline was not shed", err)
	}
	deadline, ok := dlqCtx.Deadline()
	if !ok {
		t.Fatal("dlqContext has no deadline; want a fresh bounded timeout")
	}
	if got := time.Until(deadline); got <= 0 {
		t.Fatalf("dlqContext deadline already passed (%v); want ~%v in the future", got, dlqPublishTimeout)
	}
}

// A retryable failure before the last delivery naks with the backoff delay for
// that attempt — this is the core Nak()→NakWithDelay wiring, asserting the delay
// is actually threaded through (not dropped to 0 or a bare Nak).
func TestHandleMessageNaksWithBackoffOnNonLastFailure(t *testing.T) {
	m := &fakeMsg{subject: "s.sub", meta: &jetstream.MsgMetadata{NumDelivered: 2}} // MaxDeliver 3 → not last
	w := newTestWorker(failProc, nil)

	w.handleMessage(context.Background(), m)

	if m.nakCalls != 1 {
		t.Fatalf("nakCalls = %d, want 1", m.nakCalls)
	}
	if len(m.nakDelays) != 1 || m.nakDelays[0] != 5*time.Second {
		t.Fatalf("nakDelays = %v, want [5s] (nakBackoff(2) = BackOff[1])", m.nakDelays)
	}
	if m.termCalls != 0 || m.ackCalls != 0 {
		t.Fatalf("unexpected disposition: term=%d ack=%d", m.termCalls, m.ackCalls)
	}
}

// On the final delivery a retryable failure is dead-lettered and terminated,
// not naked for another (impossible) retry.
func TestHandleMessageRoutesToDLQOnLastFailure(t *testing.T) {
	m := &fakeMsg{subject: "s.sub", meta: &jetstream.MsgMetadata{NumDelivered: 3}} // == MaxDeliver → last
	js := &fakeJetStream{}
	w := newTestWorker(failProc, js)

	w.handleMessage(context.Background(), m)

	if len(js.published) != 1 {
		t.Fatalf("DLQ published = %d, want 1", len(js.published))
	}
	if js.published[0].Subject != "dlq.s" {
		t.Fatalf("DLQ subject = %q, want dlq.s", js.published[0].Subject)
	}
	if m.termCalls != 1 {
		t.Fatalf("termCalls = %d, want 1", m.termCalls)
	}
	if m.nakCalls != 0 {
		t.Fatalf("nakCalls = %d, want 0 (retries exhausted, must not re-nak)", m.nakCalls)
	}
}

// A PermanentError is dead-lettered immediately on the first delivery — it must
// not consume retries — and its With() metadata is copied onto the DLQ message.
func TestHandleMessageRoutesPermanentErrorToDLQWithoutRetry(t *testing.T) {
	m := &fakeMsg{subject: "s.sub", meta: &jetstream.MsgMetadata{NumDelivered: 1}} // first delivery
	js := &fakeJetStream{}
	permErr := NewPermanentError(errors.New("poison")).With("reason_code", "bad_proto")
	w := newTestWorker(func(context.Context, jetstream.Msg) error { return permErr }, js)

	w.handleMessage(context.Background(), m)

	if len(js.published) != 1 {
		t.Fatalf("DLQ published = %d, want 1", len(js.published))
	}
	if got := js.published[0].Header.Get("reason_code"); got != "bad_proto" {
		t.Fatalf("DLQ header reason_code = %q, want bad_proto", got)
	}
	if m.termCalls != 1 {
		t.Fatalf("termCalls = %d, want 1", m.termCalls)
	}
	if m.nakCalls != 0 {
		t.Fatalf("nakCalls = %d, want 0 (permanent errors are never retried)", m.nakCalls)
	}
}

// When the DLQ publish itself fails the message is still terminated — data is
// lost (and counted as outcome=dropped) but the message must not loop forever.
func TestHandleMessageTermsWhenDLQPublishFails(t *testing.T) {
	m := &fakeMsg{subject: "s.sub", meta: &jetstream.MsgMetadata{NumDelivered: 3}}
	js := &fakeJetStream{publishErr: errors.New("dlq stream down")}
	w := newTestWorker(failProc, js)

	w.handleMessage(context.Background(), m)

	if len(js.published) != 1 {
		t.Fatalf("DLQ publish attempts = %d, want 1", len(js.published))
	}
	if m.termCalls != 1 {
		t.Fatalf("termCalls = %d, want 1 (must still Term on DLQ-publish failure)", m.termCalls)
	}
}

// Missing metadata is treated as the last delivery so the message routes to the
// DLQ rather than risking an endless redelivery loop (single-snapshot routing).
func TestHandleMessageTreatsMissingMetadataAsLastDelivery(t *testing.T) {
	m := &fakeMsg{subject: "s.sub", metaErr: errors.New("no metadata")}
	js := &fakeJetStream{}
	w := newTestWorker(failProc, js)

	w.handleMessage(context.Background(), m)

	if len(js.published) != 1 {
		t.Fatalf("DLQ published = %d, want 1 (missing metadata must route to DLQ)", len(js.published))
	}
	if m.nakCalls != 0 {
		t.Fatalf("nakCalls = %d, want 0", m.nakCalls)
	}
	if m.termCalls != 1 {
		t.Fatalf("termCalls = %d, want 1", m.termCalls)
	}
}

// A successful processor result acks the message.
func TestHandleMessageAcksOnSuccess(t *testing.T) {
	m := &fakeMsg{subject: "s.sub", meta: &jetstream.MsgMetadata{NumDelivered: 1}}
	w := newTestWorker(func(context.Context, jetstream.Msg) error { return nil }, nil)

	w.handleMessage(context.Background(), m)

	if m.ackCalls != 1 {
		t.Fatalf("ackCalls = %d, want 1", m.ackCalls)
	}
	if m.nakCalls != 0 || m.termCalls != 0 {
		t.Fatalf("unexpected disposition: nak=%d term=%d", m.nakCalls, m.termCalls)
	}
}

// The headline DLQ-loss fix, tested end to end: a message that fails on its LAST
// delivery while its processing context is ALREADY cancelled (e.g. it blew its
// ProcessingTimeout) must still reach the DLQ. dlqContext sheds the dead deadline
// before publishing. If that regresses (publishing on the cancelled procCtx), the
// context-aware fake records no publish and this test fails — which neither the
// isolated dlqContext unit test nor the live-context disposition tests would catch.
func TestHandleMessageReachesDLQOnAlreadyCancelledContext(t *testing.T) {
	m := &fakeMsg{subject: "s.sub", meta: &jetstream.MsgMetadata{NumDelivered: 3}} // == MaxDeliver → last
	js := &fakeJetStream{}
	w := newTestWorker(failProc, js)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled — mimics a message that exhausted ProcessingTimeout

	w.handleMessage(ctx, m)

	if len(js.published) != 1 {
		t.Fatalf("DLQ published = %d, want 1 (dlqContext must shed the cancelled deadline); publishCtxErr=%v",
			len(js.published), js.publishCtxErr)
	}
	if m.termCalls != 1 {
		t.Fatalf("termCalls = %d, want 1", m.termCalls)
	}
}

// The DLQ copy must carry the diagnostic headers an operator inspects without
// deserializing the payload, the original headers must survive (e.g. trace
// context), and the payload itself must be byte-identical. These headers are the
// debugging surface for a dead-letter, so each is pinned.
func TestPublishToDLQEnrichesHeaders(t *testing.T) {
	m := &fakeMsg{
		subject: "s.sub",
		data:    []byte("payload"),
		headers: nats.Header{"traceparent": []string{"00-abc-def-01"}},
		meta: &jetstream.MsgMetadata{
			NumDelivered: 3, // == MaxDeliver → last delivery → DLQ
			Sequence:     jetstream.SequencePair{Stream: 42, Consumer: 7},
		},
	}
	js := &fakeJetStream{}
	w := newTestWorker(failProc, js)

	w.handleMessage(context.Background(), m)

	if len(js.published) != 1 {
		t.Fatalf("DLQ published = %d, want 1", len(js.published))
	}
	pub := js.published[0]
	want := map[string]string{
		"original_subject":  "s.sub",
		"original_stream":   "s",
		"original_consumer": "c",
		"error_reason":      "boom",
		"delivery_count":    "3",
		"stream_sequence":   "42",
		"traceparent":       "00-abc-def-01", // original header preserved onto the DLQ copy
	}
	for k, v := range want {
		if got := pub.Header.Get(k); got != v {
			t.Errorf("DLQ header %q = %q, want %q", k, got, v)
		}
	}
	if pub.Header.Get("dlq_timestamp") == "" {
		t.Error("DLQ header dlq_timestamp is empty, want an RFC3339 timestamp")
	}
	if string(pub.Data) != "payload" {
		t.Errorf("DLQ data = %q, want %q (payload must be carried verbatim)", pub.Data, "payload")
	}
}
