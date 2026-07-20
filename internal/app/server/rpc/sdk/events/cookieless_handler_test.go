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

	gotIP, gotUA string
}

func (s *stubResolver) DayOf(occur time.Time) (string, bool) { return "20260720", s.dayOK }

func (s *stubResolver) DistinctID(_ context.Context, _, _, ip, ua string) (string, error) {
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
	events, err := s.resolveCookieless(ctxWithProject(context.Background()), "p1",
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

	normal := &eventsv1.Event{
		EventId:    proto.String(uuid.NewString()),
		DistinctId: proto.String("anon-u1"),
		Kind:       proto.String("page_view"),
		OccurTime:  timestamppb.New(time.Unix(1700000000, 0)),
		SessionId:  proto.String(uuid.NewString()),
	}
	resp, err := s.BatchCreate(ctxWithProject(context.Background()), cookielessReq(testCookielessEvent(), normal))
	if err != nil {
		t.Fatalf("BatchCreate: %v", err)
	}
	if got := resp.Msg.GetAccepted(); got != 1 {
		t.Errorf("accepted = %d, want 1 (cookieless dropped, normal kept)", got)
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
