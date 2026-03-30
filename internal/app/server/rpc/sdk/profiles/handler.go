package profiles

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"
	"github.com/fivebitsio/cotton/internal/app/server/rpc"
	"github.com/fivebitsio/cotton/internal/deps/nats"
	sdkprofilesv1 "github.com/fivebitsio/cotton/internal/gen/proto/sdk/profiles/v1"
	"github.com/fivebitsio/cotton/internal/gen/proto/sdk/profiles/v1/sdkprofilesv1connect"
	"github.com/fivebitsio/cotton/internal/slogx"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

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
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}

	msg := &sdkprofilesv1.ProfileIdentifyMessage{
		ExternalId: req.Msg.GetExternalId(),
		ProfileId:  req.Msg.GetProfileId(),
		ProjectId:  principal.Project.ID,
	}

	data, err := proto.Marshal(msg)
	if err != nil {
		slog.ErrorContext(ctx, "failed to marshal identify message", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, errors.New("failed to process request"))
	}

	if _, err = s.producer.Publish(ctx, nats.ProfileIdentifySubject, data); err != nil {
		slog.ErrorContext(ctx, "failed to publish identify operation to NATS", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, errors.New("failed to process request"))
	}

	return connect.NewResponse(&sdkprofilesv1.IdentifyResponse{}), nil
}

func (s *Server) Register(
	ctx context.Context,
	req *connect.Request[sdkprofilesv1.RegisterRequest],
) (*connect.Response[sdkprofilesv1.RegisterResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}

	msg := &sdkprofilesv1.ProfileRegisterMessage{
		Properties: req.Msg.GetProperties(),
		ProfileId:  req.Msg.GetProfileId(),
		ProjectId:  principal.Project.ID,
	}

	data, err := proto.Marshal(msg)
	if err != nil {
		slog.ErrorContext(ctx, "failed to marshal profile operation message", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, errors.New("failed to process request"))
	}

	if _, err = s.producer.Publish(ctx, nats.ProfileRegisterSubject, data); err != nil {
		slog.ErrorContext(ctx, "failed to publish profile operation to NATS", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, errors.New("failed to process request"))
	}

	return connect.NewResponse(&sdkprofilesv1.RegisterResponse{}), nil
}
