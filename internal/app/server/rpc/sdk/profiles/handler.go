package profiles

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/pug-sh/pug/internal/app/server/rpc"
	"github.com/pug-sh/pug/internal/deps/nats"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	sdkprofilesv1 "github.com/pug-sh/pug/internal/gen/proto/sdk/profiles/v1"
	"github.com/pug-sh/pug/internal/gen/proto/sdk/profiles/v1/sdkprofilesv1connect"
	"github.com/pug-sh/pug/internal/geo"
	"github.com/pug-sh/pug/internal/slogx"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/protobuf/proto"
)

var ipStrippedCounter metric.Int64Counter

func init() {
	meter := otel.Meter("github.com/pug-sh/pug/internal/app/server/rpc/sdk/profiles")
	ipStrippedCounter, _ = meter.Int64Counter(
		"profiles.identify_ip_stripped_total",
		metric.WithDescription("A $ip trait was stripped during Identify because the visitor IP must never be persisted. Our SDKs never send it, so a non-zero count means a hand-crafted client supplied an IP in traits."),
	)
}

type Server struct {
	sdkprofilesv1connect.UnimplementedProfilesSDKServiceHandler
	producer jetstream.JetStream
}

func NewServer(js jetstream.JetStream) *Server {
	return &Server{
		producer: js,
	}
}

func (s *Server) Identify(
	ctx context.Context,
	req *connect.Request[sdkprofilesv1.IdentifyRequest],
) (*connect.Response[sdkprofilesv1.IdentifyResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, err
	}

	// The visitor IP is personal data and must never be persisted: strip the
	// canonical $ip key from traits so an untrusted SDK caller cannot inject it
	// into a profile's stored properties (mirrors the events enrichGeo strip),
	// and count any occurrence. Scope is the canonical key our SDKs produce;
	// arbitrary client data under other keys is never interpreted as an IP.
	// delete on a nil Fields map (traits unset) is a safe no-op.
	traits := req.Msg.GetTraits().GetFields()
	if _, ok := traits[geo.PropIP]; ok {
		ipStrippedCounter.Add(ctx, 1)
	}
	delete(traits, geo.PropIP)

	msg := &sdkprofilesv1.ProfileIdentifyMessage{
		ExternalId:  proto.String(req.Msg.GetExternalId()),
		Traits:      req.Msg.GetTraits(),
		ProjectId:   proto.String(principal.Project.ID),
		AnonymousId: proto.String(req.Msg.GetAnonymousId()),
		DeviceId:    proto.String(req.Msg.GetDeviceId()),
	}

	data, err := proto.Marshal(msg)
	if err != nil {
		slog.ErrorContext(ctx, "failed to marshal identify message", slogx.Error(err),
			slog.String("project_id", principal.Project.ID))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("failed to process request"))
	}

	if _, err = s.producer.Publish(ctx, nats.ProfileIdentifySubject, data); err != nil {
		slog.ErrorContext(ctx, "failed to publish identify message", slogx.Error(err),
			slog.String("project_id", principal.Project.ID))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("failed to process request"))
	}

	return connect.NewResponse(&sdkprofilesv1.IdentifyResponse{}), nil
}
