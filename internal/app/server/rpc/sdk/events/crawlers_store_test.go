package events

import (
	"testing"
	"time"

	"connectrpc.com/connect"
	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/pug-sh/pug/internal/app/server/rpc"
	eventsworker "github.com/pug-sh/pug/internal/app/workers/events"
	"github.com/pug-sh/pug/internal/crawlerips"
	natsdep "github.com/pug-sh/pug/internal/deps/nats"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	eventsv1 "github.com/pug-sh/pug/internal/gen/proto/sdk/events/v1"
	"github.com/pug-sh/pug/internal/testutil"
)

func TestMain(m *testing.M) { testutil.Main(m) }

// A collector resends a request unchanged on a retry. It crosses NATS and the
// events worker like any event, and lands as one row with the web columns promoted.
func TestARetriedCrawlerRequestStoresOneRow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	natsTest := testutil.SetupNATS(t)
	ch := testutil.SetupClickHouse(t)
	ctx := t.Context()

	nc, err := natsgo.Connect(natsTest.URL)
	if err != nil {
		t.Fatalf("connect to NATS: %v", err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("create JetStream: %v", err)
	}

	s := NewServer(js, stubProvider{}, nil, nil, &stubRanges{verdicts: map[string]crawlerips.Verdict{"20.171.207.5": crawlerips.Verified}})
	req := visit(gptBotUA, "https://example.com/pricing?plan=pro", "20.171.207.5")
	req.OccurTime = timestamppb.New(time.Now().UTC().Truncate(time.Millisecond))
	req.AutoProperties["$status_code"] = &commonv1.PropertyValue{Value: &commonv1.PropertyValue_IntValue{IntValue: 404}}
	for range 2 {
		batch := &eventsv1.BatchCreateRequest{Events: []*eventsv1.Event{proto.CloneOf(req)}}
		if _, err := s.BatchCreate(withKey(rpc.AuthTypePrivateKey), connect.NewRequest(batch)); err != nil {
			t.Fatalf("BatchCreate: %v", err)
		}
	}

	cons, err := js.CreateConsumer(ctx, "events", jetstream.ConsumerConfig{FilterSubject: natsdep.EventsIngestSubject})
	if err != nil {
		t.Fatalf("create consumer: %v", err)
	}
	msgs, err := cons.Fetch(10, jetstream.FetchMaxWait(2*time.Second))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	processor := eventsworker.NewProcessor(ch.Conn)
	published := 0
	for msg := range msgs.Messages() {
		if err := processor.ProcessMessage(ctx, msg.Data()); err != nil {
			t.Fatalf("process: %v", err)
		}
		published++
	}
	if published != 1 {
		t.Errorf("NATS kept %d batches, want the resent one dropped", published)
	}

	var (
		rows                                    uint64
		bot                                     bool
		reason, url, pathname, hostname, kind   string
		distinctID, company, category, verified string
		status                                  *int64
	)
	if err := ch.Conn.QueryRow(ctx, `
SELECT count(), any(bot), any(bot_reason), any(url), any(pathname), any(hostname), any(kind), any(distinct_id),
       any(variantElement(auto_properties['$crawler_company'], 'String')),
       any(variantElement(auto_properties['$crawler_category'], 'String')),
       any(variantElement(auto_properties['$crawler_verified'], 'String')),
       any(variantElement(auto_properties['$status_code'], 'Int64'))
FROM events FINAL WHERE project_id = ?`, "test-project").Scan(
		&rows, &bot, &reason, &url, &pathname, &hostname, &kind, &distinctID, &company, &category, &verified, &status,
	); err != nil {
		t.Fatalf("query: %v", err)
	}
	if rows != 1 {
		t.Fatalf("stored %d rows, want 1", rows)
	}
	if !bot || reason != "GPTBot" || kind != "crawler_request" || distinctID != "crawler-gptbot" {
		t.Errorf("bot=%v bot_reason=%q kind=%q distinct_id=%q", bot, reason, kind, distinctID)
	}
	if url != "https://example.com/pricing" || pathname != "/pricing" || hostname != "example.com" {
		t.Errorf("url=%q pathname=%q hostname=%q", url, pathname, hostname)
	}
	if company != "OpenAI" || category != "training" || verified != "verified" || status == nil || *status != 404 {
		t.Errorf("company=%q category=%q verified=%q status=%v", company, category, verified, status)
	}
}
