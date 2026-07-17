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
	cdnHeaderParseFailedCounter      metric.Int64Counter
	ipStrippedCounter                metric.Int64Counter
	attributionDeriveDegradedCounter metric.Int64Counter
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
	attributionDeriveDegradedCounter, _ = meter.Int64Counter(
		"events.attribution_derive_degraded_total",
		metric.WithDescription("A $url was present on an event but did not parse as an http(s) URL with a host, so no pathname/hostname/channel could be derived. Expected to be near zero for web SDKs; a spike (reason=url_not_web) means a tenant or SDK is sending URLs the web-analytics pipeline cannot attribute."),
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
	s.enrichAttribution(ctx, projectID, events)

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
// runs last in the chain. Derivation is pure and cannot return an error, but
// pure is not infallible: a $url the parser rejects derives nothing while
// saying nothing, so — like the other enrichers — it takes ctx/projectID and
// records events.attribution_derive_degraded_total when that happens, the one
// silent-but-visible outcome (see attributionDegraded).
//
// $referrerDomain and $channel are server-only (always server-derived): the
// client value is stripped first, mirroring the bot enrichers. The remaining
// keys follow derive-if-absent / only-if-absent semantics, which Derive
// expresses by echoing a non-empty client value back unchanged — so writing
// every changed output is exactly the per-key overwrite policy. $locale is
// the one key rewritten in place (casing normalization; rollup rows are
// permanent, so fragmented variants must never reach storage).
func (s *Server) enrichAttribution(ctx context.Context, projectID string, events []*eventsv1.Event) {
	for _, event := range events {
		attribution.StripServerOnly(event.AutoProperties)

		in := attribution.InputFrom(eventProps(event.AutoProperties))
		out := attribution.Derive(in)

		if attributionDegraded(in, out) {
			attributionDeriveDegradedCounter.Add(ctx, 1, metric.WithAttributes(
				attribute.String("project_id", projectID),
				attribute.String("reason", "url_not_web"),
			))
		}

		// $locale is rewritten in place, so it is dropped AFTER Derive has read
		// it and re-added below only if it normalized to something. The write
		// loop can never clear a key — it skips empty outputs — so without this
		// a client value that NormalizeLocale blanks (whitespace-only) would
		// survive verbatim into the locale column and become a permanent
		// rollup dimension value, which is exactly what normalizing before
		// storage exists to prevent.
		//
		// Gate the drop on autoprop.String reporting a renderable value (ok):
		// that is the exact string Derive read, so a locale we could normalize
		// is the only one we may replace. A slot autoprop.String cannot render
		// (ok=false) is one the promotion layer keeps in the map (it too skips
		// on !ok); deleting it here would destroy a value storage would have
		// kept, leaving it in neither the column nor the map.
		if _, ok := autoprop.String(event.AutoProperties[attribution.PropLocale]); ok {
			delete(event.AutoProperties, attribution.PropLocale)
		}

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

// attributionDegraded reports a $url that was sent but did not parse as an
// http(s) URL with a host, so no channel — and no hostname — could be derived.
// Not an error: a native app may legitimately send a non-web $url. It keys off
// out.Channel, NOT out.Pathname: Derive sets a channel iff the URL parsed
// (classifyChannel's switch always yields at least "Direct"), whereas
// out.Pathname echoes a client-sent $pathname and stays non-empty — hiding the
// degradation — for exactly the SPA/native SDKs most likely to send a non-web
// $url. It is the one silent-but-visible outcome a pure derivation still has,
// so a spike is worth a counter.
func attributionDegraded(in attribution.Input, out attribution.Output) bool {
	return in.URL != "" && out.Channel == ""
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

// autoPropInt64 extracts a numeric auto-property ($screenWidth/$screenHeight)
// as the integer the promotion layer would store. It reads through
// autoprop.String — the same coercion — rather than switching on slots itself,
// so it renders every numeric slot (Int64, and Double as sent by a JS SDK with
// no int/double distinction) identically to storage; a private slot switch is
// exactly what silently dropped the Double slot before. Returns 0 when absent
// or when the rendered value is not a base-10 integer (a fractional or
// unparseable dim is no dim). Migration 008 mirrors this for historical rows by
// reading the Int64/String/Float64 variant slots.
func autoPropInt64(m map[string]*commonv1.PropertyValue, key string) int64 {
	s, ok := autoprop.String(m[key])
	if !ok {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
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
