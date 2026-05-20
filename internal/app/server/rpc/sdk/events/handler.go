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

var cdnHeaderParseFailedCounter metric.Int64Counter

func init() {
	meter := otel.Meter("github.com/pug-sh/pug/internal/app/server/rpc/sdk/events")
	cdnHeaderParseFailedCounter, _ = meter.Int64Counter(
		"events.cdn_header_parse_failed_total",
		metric.WithDescription("CDN-injected header could not be parsed during enrichment. The property is stripped and the event is enriched as if the header were absent."),
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
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	projectID := principal.Project.ID
	s.enrichGeo(ctx, projectID, req.Header(), events)
	s.enrichUserAgent(ctx, projectID, req.Header(), events)
	s.enrichBotScore(ctx, projectID, req.Header(), events)
	s.enrichVerifiedBot(ctx, projectID, req.Header(), events)

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
	loc := s.geoProvider.Locate(h)
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
