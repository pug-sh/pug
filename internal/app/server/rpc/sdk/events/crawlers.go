package events

import (
	"context"
	"slices"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/protobuf/proto"

	"github.com/pug-sh/pug/internal/attribution"
	"github.com/pug-sh/pug/internal/autoprop"
	"github.com/pug-sh/pug/internal/crawler"
	"github.com/pug-sh/pug/internal/crawlerips"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	eventsv1 "github.com/pug-sh/pug/internal/gen/proto/sdk/events/v1"
)

var (
	crawlerAcceptedCounter metric.Int64Counter
	crawlerDroppedCounter  metric.Int64Counter
)

func init() {
	meter := otel.Meter("github.com/pug-sh/pug/internal/app/server/rpc/sdk/events")
	crawlerAcceptedCounter, _ = meter.Int64Counter(
		"crawlers.accepted_total",
		metric.WithDescription("crawler_request events published, by category and verdict. A rising verdict=failed share means scrapers borrowing a crawler's user agent, or a collector behind a CDN reporting the CDN's address."),
	)
	crawlerDroppedCounter, _ = meter.Int64Counter(
		"crawlers.dropped_total",
		metric.WithDescription("crawler_request events not stored. reason=unknown_agent: client.user_agent names no crawler in the table or in the list's search-engine and seo entries, as when a collector's token list is newer than the server's."),
	)
}

// rangeChecker is *crawlerips.Checker; an interface so tests stub it.
type rangeChecker interface {
	Check(urls []string, ip string) crawlerips.Verdict
}

// splitCrawlerRequests takes the crawler_request events out of the batch.
func splitCrawlerRequests(events []*eventsv1.Event) (rest, requests []*eventsv1.Event) {
	isRequest := func(e *eventsv1.Event) bool { return e.GetKind() == crawler.EventKind }
	if !slices.ContainsFunc(events, isRequest) {
		return events, nil
	}
	for _, e := range events {
		if isRequest(e) {
			requests = append(requests, e)
		}
	}
	return slices.DeleteFunc(events, isRequest), requests
}

// buildCrawlerEvents turns each crawler_request into the event stored for it,
// dropping those whose user agent names no crawler.
func (s *Server) buildCrawlerEvents(ctx context.Context, projectID string, requests []*eventsv1.Event, drops dropTally) []*eventsv1.Event {
	kept := requests[:0]
	for _, e := range requests {
		agent, ok := crawler.Match(e.GetClient().GetUserAgent())
		if !ok {
			drops.add(dropReasonUnknownAgent)
			crawlerDroppedCounter.Add(ctx, 1, metric.WithAttributes(
				attribute.String("project_id", projectID),
				attribute.String("reason", dropReasonUnknownAgent),
			))
			continue
		}
		asCrawlerEvent(projectID, agent, s.ranges.Check(agent.Ranges, e.GetClient().GetIp()), e)
		kept = append(kept, e)
	}
	return kept
}

// asCrawlerEvent describes the crawler, not the site's server that sent it, so
// it carries no geo, user agent parsing, channel or UTM. Clearing client keeps
// the IP out of NATS.
func asCrawlerEvent(projectID string, agent crawler.Agent, verdict crawlerips.Verdict, e *eventsv1.Event) {
	page := withoutQuery(autoPropString(e.AutoProperties, attribution.PropURL))
	derived := attribution.Derive(attribution.Input{URL: page})
	props := map[string]*commonv1.PropertyValue{
		attribution.PropURL:          stringValue(page),
		autoprop.PropBot:             {Value: &commonv1.PropertyValue_BoolValue{BoolValue: true}},
		autoprop.PropBotReason:       stringValue(agent.Name),
		autoprop.PropCrawlerCategory: stringValue(string(agent.Category)),
		autoprop.PropCrawlerVerified: stringValue(string(verdict)),
	}
	if derived.Pathname != "" {
		props[attribution.PropPathname] = stringValue(derived.Pathname)
	}
	if derived.Hostname != "" {
		props[attribution.PropHostname] = stringValue(derived.Hostname)
	}
	if agent.Company != "" {
		props[autoprop.PropCrawlerCompany] = stringValue(agent.Company)
	}
	if status := autoPropInt64(e.AutoProperties, autoprop.PropStatusCode); status >= 100 && status <= 599 {
		props[autoprop.PropStatusCode] = &commonv1.PropertyValue{Value: &commonv1.PropertyValue_IntValue{IntValue: status}}
	}
	e.AutoProperties = props
	e.DistinctId = proto.String(agent.DistinctID())
	e.SessionId = proto.String(crawler.SessionID(projectID, agent.Name, e.GetOccurTime().AsTime()))
	e.Client = nil
}

func countCrawlerEvents(ctx context.Context, events []*eventsv1.Event) {
	for _, e := range events {
		crawlerAcceptedCounter.Add(ctx, 1, metric.WithAttributes(
			attribute.String("category", autoPropString(e.AutoProperties, autoprop.PropCrawlerCategory)),
			attribute.String("verdict", autoPropString(e.AutoProperties, autoprop.PropCrawlerVerified)),
		))
	}
}

// withoutQuery drops the query string and fragment, which can carry tokens or
// personal data.
func withoutQuery(raw string) string {
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		return raw[:i]
	}
	return raw
}

func stringValue(s string) *commonv1.PropertyValue {
	return &commonv1.PropertyValue{Value: &commonv1.PropertyValue_StringValue{StringValue: s}}
}
