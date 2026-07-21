package events

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/pug-sh/pug/internal/apperr"
	coreevents "github.com/pug-sh/pug/internal/core/events"
	eventsv1 "github.com/pug-sh/pug/internal/gen/proto/sdk/events/v1"
)

type stubResolver struct {
	dayOK    bool
	did      string
	didErr   error
	sid      string
	degraded bool

	gotIP, gotUA         string
	gotDay, gotProjectID string
}

func (s *stubResolver) DayOf(occur time.Time) (string, bool) { return "20260720", s.dayOK }

// DistinctID records EVERY argument, not just ip/ua. Discarding day and
// projectID left the four same-typed string parameters mutually
// indistinguishable to the test suite: transposing day and projectID at the call
// site kept the whole suite green while keying the salt by project — one salt per
// project, minted once, never rotating, silently destroying the daily-rotation
// privacy guarantee this package exists to provide.
func (s *stubResolver) DistinctID(_ context.Context, day, projectID, ip, ua string) (string, error) {
	s.gotDay, s.gotProjectID = day, projectID
	s.gotIP, s.gotUA = ip, ua
	return s.did, s.didErr
}

func (s *stubResolver) SessionID(_ context.Context, _, _, _ string, _ time.Time) (string, bool) {
	return s.sid, s.degraded
}

func cookielessReq(events ...*eventsv1.Event) *connect.Request[eventsv1.BatchCreateRequest] {
	req := connect.NewRequest(&eventsv1.BatchCreateRequest{Events: events})
	req.Header().Set("User-Agent", "Mozilla/5.0 TestUA")
	req.Header().Set("CF-Connecting-IP", "203.0.113.7")
	return req
}

func testCookielessEvent() *eventsv1.Event {
	return &eventsv1.Event{
		EventId:    proto.String(uuid.NewString()),
		Kind:       proto.String("page_view"),
		OccurTime:  timestamppb.New(time.Unix(1700000000, 0)),
		Cookieless: proto.Bool(true),
	}
}

// testConsentedEvent is an ordinary identified event. Batches legitimately mix
// the two (`cookieless` is per-event and both batch CEL rules are per-element),
// which is precisely why a cookieless fault must not fail the whole request.
func testConsentedEvent(distinctID string) *eventsv1.Event {
	return &eventsv1.Event{
		EventId:    proto.String(uuid.NewString()),
		DistinctId: proto.String(distinctID),
		Kind:       proto.String("page_view"),
		OccurTime:  timestamppb.New(time.Unix(1700000000, 0)),
		SessionId:  proto.String(uuid.NewString()),
	}
}

func TestResolveCookieless_FillsIdentityAndPublishes(t *testing.T) {
	js := &stubJetStream{}
	res := &stubResolver{dayOK: true, did: "cookieless-testhash", sid: "f47ac10b-58cc-4372-a567-0e02b2c3d999"}
	s := &Server{publisher: coreevents.NewPublisher(js), geoProvider: stubProvider{}, cookieless: res}

	resp, err := s.BatchCreate(ctxWithProject(context.Background()), cookielessReq(testCookielessEvent()))
	if err != nil {
		t.Fatalf("BatchCreate: %v", err)
	}
	if got := resp.Msg.GetAccepted(); got != 1 {
		t.Errorf("accepted = %d, want 1", got)
	}
	if res.gotIP != "203.0.113.7" || res.gotUA != "Mozilla/5.0 TestUA" {
		t.Errorf("resolver inputs = (%q,%q), want header-derived IP/UA", res.gotIP, res.gotUA)
	}
	var batch eventsv1.EventBatch
	if err := proto.Unmarshal(js.data, &batch); err != nil {
		t.Fatal(err)
	}
	e := batch.GetEvents()[0]
	if e.GetDistinctId() != "cookieless-testhash" || e.GetSessionId() != "f47ac10b-58cc-4372-a567-0e02b2c3d999" {
		t.Errorf("published identity = (%q,%q), want resolver-filled values", e.GetDistinctId(), e.GetSessionId())
	}
}

func TestResolveCookieless_MissingUA_RejectsBatch(t *testing.T) {
	s := &Server{geoProvider: stubProvider{}, cookieless: &stubResolver{dayOK: true, did: "cookieless-x", sid: "s"}}
	req := connect.NewRequest(&eventsv1.BatchCreateRequest{Events: []*eventsv1.Event{testCookielessEvent()}})
	req.Header().Set("CF-Connecting-IP", "203.0.113.7") // IP present, UA absent

	_, err := s.BatchCreate(ctxWithProject(context.Background()), req)
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Reason() != apperr.ReasonCookielessIdentityUnavailable {
		t.Fatalf("want ReasonCookielessIdentityUnavailable, got %v", err)
	}
}

func TestResolveCookieless_PeerAddrFallback(t *testing.T) {
	js := &stubJetStream{}
	res := &stubResolver{dayOK: true, did: "cookieless-x", sid: "f47ac10b-58cc-4372-a567-0e02b2c3d998"}
	s := &Server{publisher: coreevents.NewPublisher(js), geoProvider: stubProvider{}, cookieless: res}
	req := connect.NewRequest(&eventsv1.BatchCreateRequest{Events: []*eventsv1.Event{testCookielessEvent()}})
	req.Header().Set("User-Agent", "UA") // no proxy IP headers at all

	// connect.Request.Peer() is not settable in tests; resolveCookieless takes
	// the peer address as a plain argument, so exercise it directly.
	events, _, err := s.resolveCookieless(ctxWithProject(context.Background()), "p1",
		req.Header(), "198.51.100.4:44321", req.Msg.GetEvents())
	if err != nil {
		t.Fatalf("resolveCookieless: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("kept %d events, want 1", len(events))
	}
	if res.gotIP != "198.51.100.4" {
		t.Errorf("resolver IP = %q, want peer-derived 198.51.100.4", res.gotIP)
	}
}

func TestResolveCookieless_DayOutOfRange_DropsOnlyCookieless(t *testing.T) {
	js := &stubJetStream{}
	res := &stubResolver{dayOK: false}
	s := &Server{publisher: coreevents.NewPublisher(js), geoProvider: stubProvider{}, cookieless: res}

	resp, err := s.BatchCreate(ctxWithProject(context.Background()),
		cookielessReq(testCookielessEvent(), testConsentedEvent("anon-u1")))
	if err != nil {
		t.Fatalf("BatchCreate: %v", err)
	}
	if got := resp.Msg.GetAccepted(); got != 1 {
		t.Errorf("accepted = %d, want 1 (cookieless dropped, normal kept)", got)
	}
	// The drop is invisible without these: accepted=1 alone cannot distinguish
	// "you sent 1" from "you sent 2 and we ate one".
	if got := resp.Msg.GetDropped(); got != 1 {
		t.Errorf("dropped = %d, want 1", got)
	}
	if got := resp.Msg.GetDroppedByReason()[dropReasonDayOutOfRange]; got != 1 {
		t.Errorf("dropped_by_reason[%s] = %d, want 1 — a client must be able to tell "+
			"permanent clock skew from a retryable server fault", dropReasonDayOutOfRange, got)
	}
	var batch eventsv1.EventBatch
	if err := proto.Unmarshal(js.data, &batch); err != nil {
		t.Fatal(err)
	}
	if len(batch.GetEvents()) != 1 || batch.GetEvents()[0].GetDistinctId() != "anon-u1" {
		t.Errorf("published batch must hold only the normal event, got %v", batch.GetEvents())
	}
}

func TestResolveCookieless_SaltUnavailable_DropsOnlyCookieless(t *testing.T) {
	js := &stubJetStream{}
	res := &stubResolver{dayOK: true, didErr: errors.New("redis down")}
	s := &Server{publisher: coreevents.NewPublisher(js), geoProvider: stubProvider{}, cookieless: res}

	// A MIXED batch, which is what this test's name claims to cover: a salt
	// outage must strand the cookieless events and still publish the consented
	// ones. Sending only cookieless events would pin the 200-not-500 decision
	// while leaving "only" — the partial acceptance — unverified.
	resp, err := s.BatchCreate(ctxWithProject(context.Background()),
		cookielessReq(testCookielessEvent(), testConsentedEvent("anon-u1"), testCookielessEvent()))
	if err != nil {
		t.Fatalf("salt outage must drop cookieless events, not fail the batch: %v", err)
	}
	if got := resp.Msg.GetAccepted(); got != 1 {
		t.Errorf("accepted = %d, want 1 (consented event survives the outage)", got)
	}
	if got := resp.Msg.GetDropped(); got != 2 {
		t.Errorf("dropped = %d, want 2", got)
	}
	// salt_unavailable is a server fault, so the reason is what tells the client
	// this payload is worth retrying — the distinction accepted=1 alone erases.
	if got := resp.Msg.GetDroppedByReason()[dropReasonSaltUnavailable]; got != 2 {
		t.Errorf("dropped_by_reason[%s] = %d, want 2", dropReasonSaltUnavailable, got)
	}

	var batch eventsv1.EventBatch
	if err := proto.Unmarshal(js.data, &batch); err != nil {
		t.Fatal(err)
	}
	if len(batch.GetEvents()) != 1 || batch.GetEvents()[0].GetDistinctId() != "anon-u1" {
		t.Errorf("published batch must hold only the consented event, got %v", batch.GetEvents())
	}
}

func TestResolveCookieless_AllDropped_ReportsReasonNotBareZero(t *testing.T) {
	js := &stubJetStream{}
	res := &stubResolver{dayOK: true, didErr: errors.New("redis down")}
	s := &Server{publisher: coreevents.NewPublisher(js), geoProvider: stubProvider{}, cookieless: res}

	resp, err := s.BatchCreate(ctxWithProject(context.Background()), cookielessReq(testCookielessEvent()))
	if err != nil {
		t.Fatalf("salt outage must drop cookieless events, not fail the batch: %v", err)
	}
	if got := resp.Msg.GetAccepted(); got != 0 {
		t.Errorf("accepted = %d, want 0", got)
	}
	if js.data != nil {
		t.Error("nothing must be published for an all-dropped batch")
	}
	// The all-dropped path returns early, before publish — it must still carry the
	// tally, or a total loss looks identical to an empty request.
	if got := resp.Msg.GetDropped(); got != 1 {
		t.Errorf("dropped = %d, want 1 on the all-dropped early return", got)
	}
	if got := resp.Msg.GetDroppedByReason()[dropReasonSaltUnavailable]; got != 1 {
		t.Errorf("dropped_by_reason[%s] = %d, want 1", dropReasonSaltUnavailable, got)
	}
}

func TestResolveCookieless_NoCookielessEvents_NoResolverNeeded(t *testing.T) {
	js := &stubJetStream{}
	s := &Server{publisher: coreevents.NewPublisher(js), geoProvider: stubProvider{}} // cookieless nil
	normal := &eventsv1.Event{
		EventId:    proto.String(uuid.NewString()),
		DistinctId: proto.String("u1"),
		Kind:       proto.String("k"),
		OccurTime:  timestamppb.New(time.Unix(1700000000, 0)),
		SessionId:  proto.String(uuid.NewString()),
	}
	if _, err := s.BatchCreate(ctxWithProject(context.Background()),
		connect.NewRequest(&eventsv1.BatchCreateRequest{Events: []*eventsv1.Event{normal}})); err != nil {
		t.Fatalf("non-cookieless traffic must not touch the resolver: %v", err)
	}
}

// TestResolveCookieless_PassesDayAndProjectInOrder pins the argument order of
// DistinctID(ctx, day, projectID, ip, ua). Four same-typed strings in a row make
// any transposition invisible to the compiler, and `day` is the salt's rotation
// key: swapping it with projectID mints one salt per project that never rotates,
// which silently voids the "TTL deletion is the privacy guarantee" posture while
// every other test still passes.
func TestResolveCookieless_PassesDayAndProjectInOrder(t *testing.T) {
	js := &stubJetStream{}
	res := &stubResolver{dayOK: true, did: "cookieless-x", sid: "f47ac10b-58cc-4372-a567-0e02b2c3d997"}
	s := &Server{publisher: coreevents.NewPublisher(js), geoProvider: stubProvider{}, cookieless: res}

	req := cookielessReq(testCookielessEvent())
	if _, _, err := s.resolveCookieless(ctxWithProject(context.Background()), "proj_abc",
		req.Header(), "198.51.100.4:44321", req.Msg.GetEvents()); err != nil {
		t.Fatalf("resolveCookieless: %v", err)
	}

	for _, c := range []struct{ name, got, want string }{
		{"day", res.gotDay, "20260720"},
		{"projectID", res.gotProjectID, "proj_abc"},
		{"ip", res.gotIP, "203.0.113.7"},
		{"ua", res.gotUA, "Mozilla/5.0 TestUA"},
	} {
		if c.got != c.want {
			t.Errorf("DistinctID %s = %q, want %q (arguments transposed?)", c.name, c.got, c.want)
		}
	}
}
