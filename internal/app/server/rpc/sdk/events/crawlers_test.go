package events

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"connectrpc.com/authn"
	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"

	"github.com/pug-sh/pug/internal/app/server/rpc"
	"github.com/pug-sh/pug/internal/core/authz"
	coreevents "github.com/pug-sh/pug/internal/core/events"
	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	"github.com/pug-sh/pug/internal/crawler"
	"github.com/pug-sh/pug/internal/crawlerips"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	eventsv1 "github.com/pug-sh/pug/internal/gen/proto/sdk/events/v1"
	"github.com/pug-sh/pug/internal/gen/proto/sdk/events/v1/eventsv1connect"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
)

const (
	gptBotUA   = "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; GPTBot/1.2; +https://openai.com/gptbot)"
	gptBotJSON = "https://openai.com/gptbot.json"
)

// stubRanges answers each IP with its verdict and records what it was asked.
type stubRanges struct {
	verdicts map[string]crawlerips.Verdict
	asked    [][]string
}

func (s *stubRanges) Check(urls []string, ip string) crawlerips.Verdict {
	s.asked = append(s.asked, urls)
	if v, ok := s.verdicts[ip]; ok {
		return v
	}
	return crawlerips.Unchecked
}

func withKey(authType rpc.AuthType) context.Context {
	return authn.SetInfo(context.Background(), &rpc.Principal{
		AuthType: authType,
		Project:  &dbread.Project{ID: "test-project"},
	})
}

func visit(ua, url, ip string) *eventsv1.Event {
	e := crawlerRequest()
	e.EventId = proto.String(uuid.NewString())
	e.AutoProperties = propMap(map[string]string{"$url": url})
	e.Client = &eventsv1.Client{UserAgent: proto.String(ua), Ip: proto.String(ip)}
	return e
}

// sendCrawlerRequests sends events with a private key and returns the response
// and what reached NATS.
func sendCrawlerRequests(t *testing.T, ranges rangeChecker, authType rpc.AuthType, events ...*eventsv1.Event) (*eventsv1.BatchCreateResponse, []*eventsv1.Event) {
	t.Helper()
	js := &stubJetStream{}
	s := &Server{publisher: coreevents.NewPublisher(js), geoProvider: stubProvider{}, ranges: ranges}
	resp, err := s.BatchCreate(withKey(authType), connect.NewRequest(&eventsv1.BatchCreateRequest{Events: events}))
	if err != nil {
		t.Fatalf("BatchCreate: %v", err)
	}
	if js.data == nil {
		return resp.Msg, nil
	}
	batch := &eventsv1.EventBatch{}
	if err := proto.Unmarshal(js.data, batch); err != nil {
		t.Fatal(err)
	}
	return resp.Msg, batch.GetEvents()
}

func typedProps(e *eventsv1.Event) map[string]string {
	out := map[string]string{}
	for k, v := range e.GetAutoProperties() {
		switch x := v.GetValue().(type) {
		case *commonv1.PropertyValue_StringValue:
			out[k] = x.StringValue
		case *commonv1.PropertyValue_BoolValue:
			out[k] = "bool:" + strconv.FormatBool(x.BoolValue)
		case *commonv1.PropertyValue_IntValue:
			out[k] = "int:" + strconv.FormatInt(x.IntValue, 10)
		default:
			out[k] = "?"
		}
	}
	return out
}

func TestCrawlerRequestIsStored(t *testing.T) {
	ranges := &stubRanges{verdicts: map[string]crawlerips.Verdict{"20.171.207.5": crawlerips.Verified}}
	req := visit(gptBotUA, "https://Example.com/pricing/caf%C3%A9?utm_source=x&token=secret#plans", "20.171.207.5")
	req.AutoProperties["$status_code"] = &commonv1.PropertyValue{Value: &commonv1.PropertyValue_IntValue{IntValue: 404}}
	req.AutoProperties["$browser"] = propValue("Chrome")
	req.CustomProperties = propMap(map[string]string{"plan": "pro"})
	// BatchCreate rebuilds req in place, so compare against a copy.
	sent := proto.CloneOf(req)

	resp, got := sendCrawlerRequests(t, ranges, rpc.AuthTypePrivateKey, req)
	if resp.GetAccepted() != 1 || resp.GetDropped() != 0 || len(got) != 1 {
		t.Fatalf("response = %v, published %d events; want one accepted", resp, len(got))
	}
	e := got[0]
	if e.GetKind() != "crawler_request" || e.GetDistinctId() != "crawler-gptbot" || e.GetEventId() != sent.GetEventId() {
		t.Errorf("event = kind %q distinct_id %q event_id %q", e.GetKind(), e.GetDistinctId(), e.GetEventId())
	}
	if !e.GetOccurTime().AsTime().Equal(sent.GetOccurTime().AsTime()) {
		t.Errorf("occur_time = %v, want %v as sent", e.GetOccurTime().AsTime(), sent.GetOccurTime().AsTime())
	}
	if e.GetSessionId() != crawler.SessionID("test-project", "GPTBot", sent.GetOccurTime().AsTime()) {
		t.Errorf("session_id = %q, want the agent's session for the day", e.GetSessionId())
	}
	if e.GetClient() != nil {
		t.Errorf("client reached NATS: %v", e.GetClient())
	}
	want := map[string]string{
		"$url":              "https://Example.com/pricing/caf%C3%A9",
		"$pathname":         "/pricing/café",
		"$hostname":         "example.com",
		"$bot":              "bool:true",
		"$bot_reason":       "GPTBot",
		"$crawler_company":  "OpenAI",
		"$crawler_category": "training",
		"$crawler_verified": "verified",
		"$status_code":      "int:404",
	}
	if gotProps := typedProps(e); len(gotProps) != len(want) {
		t.Errorf("auto properties = %v, want exactly %v", gotProps, want)
	} else {
		for k, v := range want {
			if gotProps[k] != v {
				t.Errorf("%s = %q, want %q", k, gotProps[k], v)
			}
		}
	}
	if propString(e.GetCustomProperties()["plan"]) != "pro" {
		t.Errorf("custom properties = %v, want them kept", e.GetCustomProperties())
	}
	if len(ranges.asked) != 1 || len(ranges.asked[0]) != 1 || ranges.asked[0][0] != gptBotJSON {
		t.Errorf("checked against %v, want OpenAI's GPTBot file", ranges.asked)
	}
}

func TestCrawlerRequestVerdicts(t *testing.T) {
	ranges := &stubRanges{verdicts: map[string]crawlerips.Verdict{
		"20.171.207.5": crawlerips.Verified,
		"203.0.114.9":  crawlerips.Failed,
	}}
	_, got := sendCrawlerRequests(t, ranges, rpc.AuthTypePrivateKey,
		visit(gptBotUA, "https://example.com/", "20.171.207.5"),
		visit(gptBotUA, "https://example.com/", "203.0.114.9"),
		visit(gptBotUA, "https://example.com/", "10.0.0.1"),
	)
	var verdicts []string
	for _, e := range got {
		verdicts = append(verdicts, typedProps(e)["$crawler_verified"])
	}
	if strings.Join(verdicts, ",") != "verified,failed,unchecked" {
		t.Errorf("verdicts = %v", verdicts)
	}
}

func TestCrawlerRequestListEntry(t *testing.T) {
	ranges := &stubRanges{}
	_, got := sendCrawlerRequests(t, ranges, rpc.AuthTypePrivateKey,
		visit("Mozilla/5.0 (compatible; AhrefsBot/7.0; +http://ahrefs.com/robot/)", "https://example.com/blog", ""))
	props := typedProps(got[0])
	if props["$crawler_category"] != "seo" || props["$bot_reason"] != "AhrefsBot" || props["$crawler_verified"] != "unchecked" {
		t.Errorf("properties = %v", props)
	}
	for _, absent := range []string{"$crawler_company", "$status_code"} {
		if _, ok := props[absent]; ok {
			t.Errorf("%s is set for a list entry with no status: %v", absent, props)
		}
	}
	if got[0].GetDistinctId() != "crawler-ahrefsbot" {
		t.Errorf("distinct_id = %q", got[0].GetDistinctId())
	}
	if len(ranges.asked) != 1 || len(ranges.asked[0]) != 0 {
		t.Errorf("checked against %v, want no files", ranges.asked)
	}
}

func TestCrawlerRequestUnknownAgents(t *testing.T) {
	browserUA := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"
	resp, got := sendCrawlerRequests(t, &stubRanges{}, rpc.AuthTypePrivateKey,
		visit(browserUA, "https://example.com/", "198.51.100.7"),
		visit(gptBotUA, "https://example.com/", "198.51.100.7"),
		visit("curl/8.4.0", "https://example.com/", "198.51.100.7"),
	)
	if resp.GetAccepted() != 1 || resp.GetDroppedByReason()["unknown_agent"] != 2 || len(got) != 1 {
		t.Errorf("response = %v, want 1 accepted and 2 unknown_agent", resp)
	}

	resp, got = sendCrawlerRequests(t, &stubRanges{}, rpc.AuthTypePrivateKey, visit(browserUA, "https://example.com/", ""))
	if resp.GetAccepted() != 0 || resp.GetDroppedByReason()["unknown_agent"] != 1 || got != nil {
		t.Errorf("all unknown: response = %v, published %d; want nothing published", resp, len(got))
	}
}

// The rule drops a public key's crawler_request alone, not its batch.
func TestCrawlerRequestNeedsAPrivateKey(t *testing.T) {
	page := func() *eventsv1.Event {
		e := validEvent()
		e.EventId = proto.String(uuid.NewString())
		return e
	}
	resp, got := sendCrawlerRequests(t, &stubRanges{}, rpc.AuthTypePublicKey,
		page(), visit(gptBotUA, "https://example.com/", "20.171.207.5"), page())
	invalid := resp.GetInvalidEvents()
	if len(invalid) != 1 || invalid[0].GetIndex() != 1 ||
		!strings.Contains(strings.Join(invalid[0].GetViolations(), ";"), "needs a private API key") {
		t.Errorf("invalid_events = %v, want the crawler_request at index 1", invalid)
	}
	if resp.GetAccepted() != 2 || len(got) != 2 || got[0].GetKind() != "page_view" || got[1].GetKind() != "page_view" {
		t.Errorf("accepted %d, published %d; want both page views kept", resp.GetAccepted(), len(got))
	}
}

func TestCrawlerRequestStatusCode(t *testing.T) {
	for name, tt := range map[string]struct {
		sent *commonv1.PropertyValue
		want string
	}{
		"an integer":   {&commonv1.PropertyValue{Value: &commonv1.PropertyValue_IntValue{IntValue: 404}}, "int:404"},
		"a JS number":  {&commonv1.PropertyValue{Value: &commonv1.PropertyValue_DoubleValue{DoubleValue: 404}}, "int:404"},
		"a fraction":   {&commonv1.PropertyValue{Value: &commonv1.PropertyValue_DoubleValue{DoubleValue: 404.5}}, ""},
		"not a status": {&commonv1.PropertyValue{Value: &commonv1.PropertyValue_IntValue{IntValue: 99999}}, ""},
	} {
		req := visit(gptBotUA, "https://example.com/", "")
		req.AutoProperties["$status_code"] = tt.sent
		_, got := sendCrawlerRequests(t, &stubRanges{}, rpc.AuthTypePrivateKey, req)
		if len(got) != 1 {
			t.Fatalf("%s: published %d events, want 1", name, len(got))
		}
		if status := typedProps(got[0])["$status_code"]; status != tt.want {
			t.Errorf("%s: $status_code = %q, want %q", name, status, tt.want)
		}
	}
}

// The batch's other events are enriched as usual, and enrichment leaves the
// crawler event's server-set properties alone.
func TestCrawlerRequestsJoinTheBatch(t *testing.T) {
	server := validEvent()
	server.AutoProperties = propMap(map[string]string{"$url": "https://example.com/checkout?utm_source=mail"})
	_, got := sendCrawlerRequests(t, &stubRanges{}, rpc.AuthTypePrivateKey, visit(gptBotUA, "https://example.com/", ""), server)
	if len(got) != 2 {
		t.Fatalf("published %d events, want both", len(got))
	}
	for _, e := range got {
		props := typedProps(e)
		switch e.GetKind() {
		case "crawler_request":
			if props["$bot"] != "bool:true" || props["$crawler_category"] != "training" {
				t.Errorf("crawler event lost its properties: %v", props)
			}
		default:
			if e.GetDistinctId() != "anon-u1" || props["$utmSource"] != "mail" || props["$bot"] != "" {
				t.Errorf("server event = distinct_id %q, properties %v", e.GetDistinctId(), props)
			}
		}
	}
}

func TestWithoutQuery(t *testing.T) {
	for in, want := range map[string]string{
		"https://example.com/a?b=1#c": "https://example.com/a",
		"https://example.com/a#c?b=1": "https://example.com/a",
		"https://example.com/a?":      "https://example.com/a",
		"https://example.com/":        "https://example.com/",
	} {
		if got := withoutQuery(in); got != want {
			t.Errorf("withoutQuery(%q) = %q, want %q", in, got, want)
		}
	}
}

type stubKeyLookup struct{}

func (stubKeyLookup) GetProjectByPublicApiKey(_ context.Context, key string) (dbread.Project, error) {
	if key == "pub_served" {
		return dbread.Project{ID: "proj-test"}, nil
	}
	return dbread.Project{}, pgx.ErrNoRows
}

func (stubKeyLookup) GetProjectByPrivateApiKey(_ context.Context, key string) (dbread.Project, error) {
	if key == "prv_served" {
		return dbread.Project{ID: "proj-test"}, nil
	}
	return dbread.Project{}, pgx.ErrNoRows
}

func (stubKeyLookup) InvalidateProjectKeys(context.Context, string, ...string) {}

type stubRoleLookup struct{}

func (stubRoleLookup) GetMemberRole(context.Context, string, string) (coreorgs.Role, error) {
	return coreorgs.Role(""), errors.New("role lookup must not run on an API-key request")
}

// The collector's JSON through the production interceptors.
func TestCrawlerRequestOverJSON(t *testing.T) {
	authorizer, err := authz.NewAuthorizer()
	if err != nil {
		t.Fatal(err)
	}
	otelInterceptor, err := otelconnect.NewInterceptor()
	if err != nil {
		t.Fatal(err)
	}
	js := &stubJetStream{}
	path, handler := eventsv1connect.NewEventsServiceHandler(NewServer(js, stubProvider{}, nil, nil, &stubRanges{}),
		rpc.HandlerOptions(otelInterceptor, authorizer, stubRoleLookup{}))
	mux := http.NewServeMux()
	mux.Handle(path, authn.NewMiddleware(rpc.WithSDKAuth(stubKeyLookup{})).Wrap(handler))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	request := func(url string) string {
		return `{"eventId": "` + "0193b2c4-8f1e-7a2b-9c3d-4e5f6a7b8c9d" + `", "occurTime": "2026-11-14T09:30:00Z",
			"kind": "crawler_request",
			"client": {"userAgent": "` + gptBotUA + `", "ip": "20.171.207.5"},
			"autoProperties": {"$url": {"stringValue": "` + url + `"}, "$status_code": {"intValue": 200}}}`
	}
	post := func(key, body string) (int, map[string]any) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+path+"BatchCreate", strings.NewReader(`{"events": [`+body+`]}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("content-type", "application/json")
		req.Header.Set("x-api-key", key)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	for name, url := range map[string]string{
		"a plain url":     "https://example.com/pricing",
		"the longest url": "https://example.com/" + strings.Repeat("a", 1004),
		"an odd path":     "https://example.com/%c0%ae%c0%ae/a|b[1]^c",
	} {
		js.data = nil
		if status, body := post("prv_served", request(url)); status != http.StatusOK || body["accepted"] != float64(1) || js.data == nil {
			t.Errorf("%s: %d %v, published %v", name, status, body, js.data != nil)
		}
	}

	js.data = nil
	status, body := post("prv_served", request("https://example.com/"+strings.Repeat("a", 1005)))
	if status != http.StatusOK || body["dropped"] != float64(1) || js.data != nil {
		t.Errorf("a url over 1024 characters: %d %v, want it dropped as invalid", status, body)
	}
	status, body = post("pub_served", request("https://example.com/pricing"))
	if status != http.StatusOK || body["dropped"] != float64(1) || js.data != nil {
		t.Errorf("a public key: %d %v, want it dropped as invalid", status, body)
	}
}
