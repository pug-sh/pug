package events

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/pug-sh/pug/internal/apperr"
	"github.com/pug-sh/pug/internal/cookieless"
	coreevents "github.com/pug-sh/pug/internal/core/events"
	eventsv1 "github.com/pug-sh/pug/internal/gen/proto/sdk/events/v1"
)

type stubResolver struct {
	dayOK    bool
	did      string
	didErr   error
	sid      string
	degraded cookieless.DegradeReason

	gotIP, gotUA  string
	gotProjectID  string
	gotDay        cookieless.Day
	gotDays       []cookieless.Day
	distinctCalls int
}

// DayOf derives the day from occur rather than returning a constant. A fixed day
// made a two-day batch unrepresentable in tests, so the per-day memoisation in
// resolveCookieless — whose comment promises "a batch can legitimately straddle
// UTC midnight (two days, two salts, two ids)" — could not be exercised, and
// collapsing it to a single batch-wide key passed.
func (s *stubResolver) DayOf(occur time.Time) (cookieless.Day, bool) {
	return cookieless.Day(occur.UTC().Format("20060102")), s.dayOK
}

// DistinctID records EVERY argument, not just ip/ua. Discarding day and
// projectID left the four same-typed string parameters mutually
// indistinguishable to the test suite: transposing day and projectID at the call
// site kept the whole suite green while keying the salt by project — one salt per
// project, minted once, never rotating, silently destroying the daily-rotation
// privacy guarantee this package exists to provide.
func (s *stubResolver) DistinctID(_ context.Context, day cookieless.Day, projectID, ip, ua string) (string, error) {
	s.gotDay, s.gotProjectID = day, projectID
	s.gotIP, s.gotUA = ip, ua
	s.gotDays = append(s.gotDays, day)
	s.distinctCalls++
	return s.did, s.didErr
}

func (s *stubResolver) SessionID(_ context.Context, _, _ string, _ cookieless.Day, _ time.Time) (string, cookieless.DegradeReason) {
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

// TestResolveCookieless_PassesArgumentsInOrder guards the call site's argument
// order. Its day/projectID half is now enforced by the compiler — cookieless.Day
// is a distinct type, so transposing those two no longer builds — but ip and ua
// are both plain strings and remain transposable in silence, producing a real id
// derived from the wrong inputs.
func TestResolveCookieless_PassesArgumentsInOrder(t *testing.T) {
	js := &stubJetStream{}
	res := &stubResolver{dayOK: true, did: "cookieless-x", sid: "f47ac10b-58cc-4372-a567-0e02b2c3d997"}
	s := &Server{publisher: coreevents.NewPublisher(js), geoProvider: stubProvider{}, cookieless: res}

	req := cookielessReq(testCookielessEvent())
	if _, _, err := s.resolveCookieless(ctxWithProject(context.Background()), "proj_abc",
		req.Header(), "198.51.100.4:44321", req.Msg.GetEvents()); err != nil {
		t.Fatalf("resolveCookieless: %v", err)
	}

	// testCookielessEvent occurs at unix 1700000000 = 2023-11-14T22:13:20Z.
	if res.gotDay != cookieless.Day("20231114") {
		t.Errorf("DistinctID day = %q, want %q", res.gotDay, "20231114")
	}
	for _, c := range []struct{ name, got, want string }{
		{"projectID", res.gotProjectID, "proj_abc"},
		{"ip", res.gotIP, "203.0.113.7"},
		{"ua", res.gotUA, "Mozilla/5.0 TestUA"},
	} {
		if c.got != c.want {
			t.Errorf("DistinctID %s = %q, want %q (arguments transposed?)", c.name, c.got, c.want)
		}
	}
}

// TestResolveCookieless_BatchStraddlingMidnightGetsTwoIDs exercises the per-day
// memoisation. resolveCookieless caches the derived id per day precisely because
// "a batch can legitimately straddle UTC midnight (two days, two salts, two
// ids)" — and collapsing that cache to one batch-wide key used to pass, because
// the stub returned a constant day and no test could express a two-day batch.
//
// Merging the two days would hand both events one id across the rotation
// boundary, which is exactly the linkage the daily rotation exists to prevent.
func TestResolveCookieless_BatchStraddlingMidnightGetsTwoIDs(t *testing.T) {
	js := &stubJetStream{}
	res := &stubResolver{dayOK: true, did: "cookieless-x", sid: "f47ac10b-58cc-4372-a567-0e02b2c3d997"}
	s := &Server{publisher: coreevents.NewPublisher(js), geoProvider: stubProvider{}, cookieless: res}

	before := testCookielessEvent()
	before.OccurTime = timestamppb.New(time.Date(2026, 7, 20, 23, 59, 0, 0, time.UTC))
	after := testCookielessEvent()
	after.OccurTime = timestamppb.New(time.Date(2026, 7, 21, 0, 1, 0, 0, time.UTC))

	req := cookielessReq(before, after)
	kept, _, err := s.resolveCookieless(ctxWithProject(context.Background()), "proj_abc",
		req.Header(), "198.51.100.4:44321", req.Msg.GetEvents())
	if err != nil {
		t.Fatalf("resolveCookieless: %v", err)
	}
	if len(kept) != 2 {
		t.Fatalf("kept %d events, want 2", len(kept))
	}
	if res.distinctCalls != 2 {
		t.Errorf("DistinctID called %d times, want 2 — a batch spanning midnight needs one id per day", res.distinctCalls)
	}
	if len(res.gotDays) == 2 && res.gotDays[0] == res.gotDays[1] {
		t.Errorf("both events resolved under day %q — the two sides of midnight were merged", res.gotDays[0])
	}
}

// TestResolveCookieless_SameDayBatchReusesOneID is the memo's other half: within
// one day the batch must derive the id once, not once per event.
func TestResolveCookieless_SameDayBatchReusesOneID(t *testing.T) {
	js := &stubJetStream{}
	res := &stubResolver{dayOK: true, did: "cookieless-x", sid: "f47ac10b-58cc-4372-a567-0e02b2c3d997"}
	s := &Server{publisher: coreevents.NewPublisher(js), geoProvider: stubProvider{}, cookieless: res}

	a, b := testCookielessEvent(), testCookielessEvent()
	req := cookielessReq(a, b)
	if _, _, err := s.resolveCookieless(ctxWithProject(context.Background()), "proj_abc",
		req.Header(), "198.51.100.4:44321", req.Msg.GetEvents()); err != nil {
		t.Fatalf("resolveCookieless: %v", err)
	}
	if res.distinctCalls != 1 {
		t.Errorf("DistinctID called %d times for a single-day batch, want 1 (memo not reused)", res.distinctCalls)
	}
}

// TestResolveCookieless_DegradedSessionStillPublishes pins the degraded contract:
// a Redis session outage coarsens sessionization but must never drop the event.
// The stub carried a degraded field that no test ever set, so the handler's
// degraded branch was never executed.
func TestResolveCookieless_DegradedSessionStillPublishes(t *testing.T) {
	js := &stubJetStream{}
	res := &stubResolver{
		dayOK:    true,
		did:      "cookieless-x",
		sid:      "f47ac10b-58cc-4372-a567-0e02b2c3d997",
		degraded: cookieless.DegradeGetFailed,
	}
	s := &Server{publisher: coreevents.NewPublisher(js), geoProvider: stubProvider{}, cookieless: res}

	req := cookielessReq(testCookielessEvent())
	kept, drops, err := s.resolveCookieless(ctxWithProject(context.Background()), "proj_abc",
		req.Header(), "198.51.100.4:44321", req.Msg.GetEvents())
	if err != nil {
		t.Fatalf("a degraded session must not fail the request: %v", err)
	}
	if len(kept) != 1 {
		t.Fatalf("kept %d events, want 1 — degraded means coarser sessions, never lost data", len(kept))
	}
	if drops.total() != 0 {
		t.Errorf("dropped %d events on a degraded session, want 0", drops.total())
	}
	if kept[0].GetSessionId() != res.sid {
		t.Errorf("session id = %q, want the fallback %q", kept[0].GetSessionId(), res.sid)
	}
}

// TestResolveCookieless_CorruptSaltReportsDistinctReason pins the CONSUMER half
// of ErrCorruptSalt. The producer half — storeSalt rejecting an undecodable or
// wrong-length value — was already pinned; this branch was not. Deleting the
// errors.Is from resolveCookieless left the entire package green, so a corrupt
// salt would have been reported to operators and clients as salt_unavailable.
//
// That distinction is the whole reason the sentinel exists. salt_unavailable
// means "retry may succeed"; retry is exactly wrong here, because SETNX only
// mints when the key is absent, so nothing ever overwrites a corrupt value and it
// is re-read and re-rejected until the key expires. One reason says wait, the
// other says page a human.
//
// The stub returns a WRAPPED sentinel rather than the bare one, so this pins
// errors.Is rather than ==: a future rewrap that breaks the chain fails here.
func TestResolveCookieless_CorruptSaltReportsDistinctReason(t *testing.T) {
	js := &stubJetStream{}
	res := &stubResolver{
		dayOK:  true,
		didErr: fmt.Errorf("cookieless: fetch salt for 20260721: %w", cookieless.ErrCorruptSalt),
	}
	s := &Server{publisher: coreevents.NewPublisher(js), geoProvider: stubProvider{}, cookieless: res}

	resp, err := s.BatchCreate(ctxWithProject(context.Background()),
		cookielessReq(testCookielessEvent(), testConsentedEvent("anon-u1")))
	if err != nil {
		t.Fatalf("BatchCreate: %v", err)
	}
	if got := resp.Msg.GetDroppedByReason()[dropReasonSaltCorrupt]; got != 1 {
		t.Errorf("dropped_by_reason[%s] = %d, want 1 — a corrupt salt must not be "+
			"reported as the retryable reason", dropReasonSaltCorrupt, got)
	}
	if got := resp.Msg.GetDroppedByReason()[dropReasonSaltUnavailable]; got != 0 {
		t.Errorf("dropped_by_reason[%s] = %d, want 0 — retrying never clears a corrupt salt",
			dropReasonSaltUnavailable, got)
	}
	if got := resp.Msg.GetAccepted(); got != 1 {
		t.Errorf("accepted = %d, want 1 — consented traffic is unaffected by a salt fault", got)
	}
}

// TestDropReasons_RegistryMatchesDeclaredConstants pins the drop-reason
// vocabulary against ADDITION — which is precisely what its predecessor could
// not do.
//
// The reason strings ship to clients as keys of
// BatchCreateResponse.dropped_by_reason, and the proto comment invites branching
// on them (salt_unavailable is retryable, day_out_of_range is not). The wire type
// is map<string, uint32> — protobuf forbids enum map keys — so clients get no
// generated symbol and the valid set exists only as these constants plus prose.
//
// The previous version of this test asserted nothing. It built a 4-entry map
// literal from the constants, then checked `len(want) != 4` — unreachable, since
// duplicate constant keys in a Go map literal are a COMPILE error, so the literal
// is always exactly 4 — and a duplicate check over map keys, also unreachable
// since ranging a map yields unique keys. Only the empty-string check was live.
// Adding and emitting a fifth reason left it green. Worse, because salt_corrupt
// appeared in its table the reason READ as covered, while its consumer branch
// (the errors.Is in resolveCookieless) was in fact unpinned — deleting that
// branch kept the whole package green. See TestResolveCookieless_CorruptSaltReportsDistinctReason.
//
// This version reads the declarations out of the source with go/ast, so a new
// dropReason* constant that is never registered in allDropReasons fails here.
func TestDropReasons_RegistryMatchesDeclaredConstants(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "handler.go", nil, 0)
	if err != nil {
		t.Fatalf("parse handler.go: %v", err)
	}

	declared := map[string]string{}
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "dropReason") || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote %s: %v", name.Name, err)
				}
				declared[name.Name] = v
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("found no dropReason* constants in handler.go — the AST scan broke, not the code")
	}

	registered := map[string]bool{}
	for _, r := range allDropReasons {
		if r == "" {
			t.Error("a drop reason must never be empty — it becomes a metric label and a wire key")
		}
		if registered[r] {
			t.Errorf("duplicate drop reason %q: the metric label and the wire key would collide", r)
		}
		registered[r] = true
	}

	for name, value := range declared {
		if !registered[value] {
			t.Errorf("const %s = %q is declared but missing from allDropReasons; "+
				"register it and document its retry disposition", name, value)
		}
	}
	if len(registered) != len(declared) {
		t.Errorf("allDropReasons has %d entries but handler.go declares %d dropReason* constants",
			len(registered), len(declared))
	}
}

// TestPeerIP covers the two fallbacks that only fire without a proxy header.
// peerIP is the last resort for the cookieless HMAC's ip input, so returning the
// wrong string here does not fail — it silently derives a different visitor.
func TestPeerIP(t *testing.T) {
	for _, c := range []struct{ name, addr, want string }{
		{"host_port", "198.51.100.4:44321", "198.51.100.4"},
		{"ipv6_host_port", "[2001:db8::1]:44321", "2001:db8::1"},
		{"empty", "", ""},
		{"bare_ipv4_no_port", "198.51.100.4", "198.51.100.4"},
		{"bare_ipv6_no_port", "2001:db8::1", "2001:db8::1"},
		// The peer host keys an HMAC exactly like a header-derived IP, so it must
		// clear the same bar. geo.ClientIP was given parsing precisely so the
		// NUL-framing argument in DistinctID would hold by construction rather
		// than by transport accident — but this path bypassed it:
		// net.SplitHostPort("198.51.100.4\x00x:44321") returns that host with a
		// nil error, and the no-port branch returns the string verbatim. Safe only
		// while RemoteAddr is kernel-derived; a Unix socket, an h2c shim or a
		// proxy-protocol listener reintroduces an unvalidated hash input.
		// Rejecting here routes it into the existing loud ip == "" refusal.
		{"nul_in_host_rejected", "198.51.100.4\x00x:44321", ""},
		{"nul_bare_no_port_rejected", "198.51.100.4\x00x", ""},
		{"garbage_rejected", "not-an-ip", ""},
		{"canonicalised_ipv6", "[2001:0DB8::0001]:443", "2001:db8::1"},
		{"zone_dropped", "[fe80::1%eth0]:443", "fe80::1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := peerIP(c.addr); got != c.want {
				t.Errorf("peerIP(%q) = %q, want %q", c.addr, got, c.want)
			}
		})
	}
}
