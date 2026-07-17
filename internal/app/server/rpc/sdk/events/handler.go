package events

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/protobuf/proto"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/pug-sh/pug/internal/app/server/rpc"
	"github.com/pug-sh/pug/internal/apperr"
	"github.com/pug-sh/pug/internal/attribution"
	"github.com/pug-sh/pug/internal/autoprop"
	coreevents "github.com/pug-sh/pug/internal/core/events"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	eventsv1 "github.com/pug-sh/pug/internal/gen/proto/sdk/events/v1"
	"github.com/pug-sh/pug/internal/gen/proto/sdk/events/v1/eventsv1connect"
	"github.com/pug-sh/pug/internal/geo"
	"github.com/pug-sh/pug/internal/slogx"
	"github.com/pug-sh/pug/internal/useragent"
)

const (
	cfHeaderBotScore    = "CF-Bot-Score"
	cfHeaderVerifiedBot = "CF-Verified-Bot"
)

var (
	cdnHeaderParseFailedCounter metric.Int64Counter
	ipStrippedCounter           metric.Int64Counter
)

func init() {
	meter := otel.Meter("github.com/pug-sh/pug/internal/app/server/rpc/sdk/events")
	cdnHeaderParseFailedCounter, _ = meter.Int64Counter(
		"events.cdn_header_parse_failed_total",
		metric.WithDescription("CDN-injected header could not be parsed during enrichment. The property is stripped and the event is enriched as if the header were absent."),
	)
	ipStrippedCounter, _ = meter.Int64Counter(
		"events.ip_stripped_total",
		metric.WithDescription("A $ip auto-property was stripped during geo enrichment because the visitor IP must never be persisted. Our SDKs never send it, so a non-zero count means a hand-crafted client supplied one (source=client) or a provider emitted one (source=provider) — both are contract violations worth investigating."),
	)
}

type Server struct {
	eventsv1connect.UnimplementedEventsServiceHandler
	publisher   *coreevents.Publisher
	geoProvider geo.Provider
	uaParser    *useragent.Parser
}

func NewServer(producer jetstream.JetStream, geoProvider geo.Provider, uaParser *useragent.Parser) *Server {
	return &Server{
		publisher:   coreevents.NewPublisher(producer),
		geoProvider: geoProvider,
		uaParser:    uaParser,
	}
}

func (s *Server) BatchCreate(
	ctx context.Context,
	req *connect.Request[eventsv1.BatchCreateRequest],
) (*connect.Response[eventsv1.BatchCreateResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, rpc.ConnectCtxErr(err)
	}

	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, err
	}

	events := req.Msg.GetEvents()
	if len(events) == 0 {
		slog.DebugContext(ctx, "received empty event batch",
			slog.String("project_id", principal.Project.ID))
		return connect.NewResponse(&eventsv1.BatchCreateResponse{Accepted: proto.Uint32(0)}), nil
	}

	if err := coreevents.ValidateExternalEvents(events); err != nil {
		return nil, apperr.Invalid(apperr.ReasonInvalidEventBatch, err.Error())
	}

	projectID := principal.Project.ID
	s.enrichGeo(ctx, projectID, req.Header(), events)
	s.enrichUserAgent(ctx, projectID, req.Header(), events)
	s.enrichBotScore(ctx, projectID, req.Header(), events)
	s.enrichVerifiedBot(ctx, projectID, req.Header(), events)
	s.enrichAttribution(events)

	if err := s.publisher.Publish(ctx, principal.Project.ID, events); err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("failed to accept events"))
	}

	return connect.NewResponse(&eventsv1.BatchCreateResponse{
		Accepted: proto.Uint32(uint32(len(events))),
	}), nil
}

func (s *Server) enrichUserAgent(ctx context.Context, projectID string, h http.Header, events []*eventsv1.Event) {
	if s.uaParser == nil {
		slog.WarnContext(ctx, "user-agent enrichment skipped: parser not initialized", slog.String("project_id", projectID))
		return
	}
	props := s.uaParser.Parse(h)
	if len(props) == 0 {
		slog.DebugContext(ctx, "user-agent enrichment skipped",
			slog.String("project_id", projectID),
			slog.Bool("header_present", h.Get("User-Agent") != ""))
		return
	}
	for _, event := range events {
		if event.AutoProperties == nil {
			event.AutoProperties = make(map[string]*commonv1.PropertyValue, len(props))
		}
		for k, v := range props {
			if _, exists := event.AutoProperties[k]; !exists {
				event.AutoProperties[k] = autoprop.PropertyValue(ctx, projectID, k, v)
			}
		}
	}
}

func (s *Server) enrichGeo(ctx context.Context, projectID string, h http.Header, events []*eventsv1.Event) {
	// The visitor IP is personal data and must never be persisted: strip the
	// canonical $ip key from every event so it can never reach NATS/ClickHouse,
	// and count any occurrence. The strip targets the canonical key our SDKs and
	// enrichment use; a hostile caller can still place arbitrary data under other
	// keys, which no strip can prevent and which is never read back as an IP. The
	// geo provider uses the IP only transiently for lookup and does not emit it.
	for _, event := range events {
		if _, ok := event.AutoProperties[geo.PropIP]; ok {
			ipStrippedCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("source", "client")))
		}
		delete(event.AutoProperties, geo.PropIP)
	}

	loc := s.geoProvider.Locate(h)
	// Defensive: never let a provider leak $ip into storage. The current
	// Cloudflare provider can't, but a future IP-lookup provider might regress —
	// the counter makes that regression visible instead of silent.
	if _, ok := loc[geo.PropIP]; ok {
		ipStrippedCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("source", "provider")))
	}
	delete(loc, geo.PropIP)
	if len(loc) == 0 {
		slog.DebugContext(ctx, "geo location empty, skipping enrichment", slog.String("project_id", projectID))
		return
	}
	for _, event := range events {
		if event.AutoProperties == nil {
			event.AutoProperties = make(map[string]*commonv1.PropertyValue, len(loc))
		}
		for k, v := range loc {
			event.AutoProperties[k] = autoprop.PropertyValue(ctx, projectID, k, v)
		}
	}
}

func (s *Server) enrichBotScore(ctx context.Context, projectID string, h http.Header, events []*eventsv1.Event) {
	// Always strip client-supplied bot score (server-only property).
	for _, event := range events {
		delete(event.AutoProperties, autoprop.PropBotScore)
	}

	botScoreStr := h.Get(cfHeaderBotScore)
	if botScoreStr == "" {
		return
	}

	val, err := strconv.ParseUint(botScoreStr, 10, 8)
	if err != nil {
		slog.WarnContext(ctx, "failed to parse bot score from CDN header",
			slogx.Error(err),
			slog.String("project_id", projectID),
			slog.String("bot_score", botScoreStr),
			slog.Int("batch_size", len(events)))
		cdnHeaderParseFailedCounter.Add(ctx, 1, metric.WithAttributes(
			attribute.String("project_id", projectID),
			attribute.String("header", cfHeaderBotScore),
		))
		return
	}

	for _, event := range events {
		if event.AutoProperties == nil {
			event.AutoProperties = make(map[string]*commonv1.PropertyValue)
		}
		event.AutoProperties[autoprop.PropBotScore] = &commonv1.PropertyValue{
			Value: &commonv1.PropertyValue_IntValue{IntValue: int64(val)},
		}
	}
}

// enrichAttribution derives the web-analytics auto-properties ($pathname,
// $hostname, $referrerDomain, $channel, $screenSize, UTM completion from the
// URL query, $locale normalization) from the client-sent navigation
// properties via attribution.Derive. Header-independent and per-event, so it
// runs last in the chain. Unlike the other enrichers it has no failure path:
// derivation is pure, and absent inputs simply derive nothing.
//
// $referrerDomain and $channel are server-only (always server-derived): the
// client value is stripped first, mirroring the bot enrichers. The remaining
// keys follow derive-if-absent / only-if-absent semantics, which Derive
// expresses by echoing a non-empty client value back unchanged — so writing
// every changed output is exactly the per-key overwrite policy. $locale is
// the one key rewritten in place (casing normalization; rollup rows are
// permanent, so fragmented variants must never reach storage).
func (s *Server) enrichAttribution(events []*eventsv1.Event) {
	for _, event := range events {
		for _, key := range attribution.ServerOnlyKeys {
			delete(event.AutoProperties, key)
		}

		out := attribution.Derive(attribution.InputFrom(eventProps(event.AutoProperties)))

		// $locale is rewritten in place, so it is dropped AFTER Derive has read
		// it and re-added below only if it normalized to something. The write
		// loop can never clear a key — it skips empty outputs — so without this
		// a client value that NormalizeLocale blanks (whitespace-only) would
		// survive verbatim into the locale column and become a permanent
		// rollup dimension value, which is exactly what normalizing before
		// storage exists to prevent.
		delete(event.AutoProperties, attribution.PropLocale)

		for _, p := range out.Pairs() {
			if p.Value == "" || autoPropString(event.AutoProperties, p.Key) == p.Value {
				continue
			}
			if event.AutoProperties == nil {
				event.AutoProperties = make(map[string]*commonv1.PropertyValue)
			}
			event.AutoProperties[p.Key] = &commonv1.PropertyValue{
				Value: &commonv1.PropertyValue_StringValue{StringValue: p.Value},
			}
		}
	}
}

// eventProps adapts an event's auto-property map to attribution.Source.
type eventProps map[string]*commonv1.PropertyValue

func (p eventProps) String(key string) string { return autoPropString(p, key) }

func (p eventProps) ScreenDims() (int64, int64) {
	return autoPropInt64(p, autoprop.PropScreenWidth), autoPropInt64(p, autoprop.PropScreenHeight)
}

// autoPropString extracts an auto-property as the string the promotion layer
// would store, so derivation sees exactly what would otherwise land in the
// column — autoprop.String is that same coercion, shared rather than mirrored.
func autoPropString(m map[string]*commonv1.PropertyValue, key string) string {
	s, _ := autoprop.String(m[key])
	return s
}

// autoPropInt64 extracts a numeric auto-property ($screenWidth/$screenHeight
// arrive as Int64 slots via autoprop; a string slot is parsed as fallback).
// Returns 0 when absent or unparseable.
func autoPropInt64(m map[string]*commonv1.PropertyValue, key string) int64 {
	pv, ok := m[key]
	if !ok || pv == nil {
		return 0
	}
	switch v := pv.GetValue().(type) {
	case *commonv1.PropertyValue_IntValue:
		return v.IntValue
	case *commonv1.PropertyValue_StringValue:
		n, err := strconv.ParseInt(v.StringValue, 10, 64)
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

func (s *Server) enrichVerifiedBot(ctx context.Context, projectID string, h http.Header, events []*eventsv1.Event) {
	// Always strip client-supplied verified-bot flag (server-only property).
	for _, event := range events {
		delete(event.AutoProperties, autoprop.PropVerifiedBot)
	}

	raw := h.Get(cfHeaderVerifiedBot)
	if raw == "" {
		return
	}
	if raw != "true" && raw != "false" {
		slog.WarnContext(ctx, "unexpected CF-Verified-Bot header value, skipping enrichment",
			slog.String("project_id", projectID),
			slog.String("verified_bot", raw),
			slog.Int("batch_size", len(events)))
		cdnHeaderParseFailedCounter.Add(ctx, 1, metric.WithAttributes(
			attribute.String("project_id", projectID),
			attribute.String("header", cfHeaderVerifiedBot),
		))
		return
	}
	verified := raw == "true"

	for _, event := range events {
		if event.AutoProperties == nil {
			event.AutoProperties = make(map[string]*commonv1.PropertyValue)
		}
		event.AutoProperties[autoprop.PropVerifiedBot] = &commonv1.PropertyValue{
			Value: &commonv1.PropertyValue_BoolValue{BoolValue: verified},
		}
	}
}
