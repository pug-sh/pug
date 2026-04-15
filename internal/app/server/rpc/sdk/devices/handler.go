package devices

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"

	"github.com/fivebitsio/cotton/internal/app/server/rpc"
	"github.com/fivebitsio/cotton/internal/deps/nats"
	"github.com/fivebitsio/cotton/internal/deps/telemetry"
	devicesv1 "github.com/fivebitsio/cotton/internal/gen/proto/sdk/devices/v1"
	"github.com/fivebitsio/cotton/internal/slogx"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

type Server struct {
	producer jetstream.JetStream
}

func NewServer(js jetstream.JetStream) *Server {
	return &Server{
		producer: js,
	}
}

func (s *Server) Subscribe(
	ctx context.Context,
	req *connect.Request[devicesv1.SubscribeRequest],
) (*connect.Response[devicesv1.SubscribeResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}

	msg := &devicesv1.DeviceOperationMessage{
		DeviceId:  proto.String(req.Msg.GetDeviceId()),
		ProjectId: proto.String(principal.Project.ID),
		OperationPayload: &devicesv1.DeviceOperationMessage_Subscribe{
			Subscribe: &devicesv1.SubscribePayload{
				Platform:          proto.String(req.Msg.GetPlatform()),
				ProfileExternalId: proto.String(req.Msg.GetProfileExternalId()),
				ProfileId:         proto.String(req.Msg.GetProfileId()),
				Token:             proto.String(req.Msg.GetToken()),
				Properties:        req.Msg.GetProperties(),
			},
		},
	}

	data, err := proto.Marshal(msg)
	if err != nil {
		slog.ErrorContext(ctx, "failed to marshal subscribe message", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("failed to process request"))
	}

	if _, err = s.producer.Publish(ctx, nats.DeviceOpsSubject, data); err != nil {
		slog.ErrorContext(ctx, "failed to publish subscribe operation to NATS", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("failed to process request"))
	}

	return connect.NewResponse(&devicesv1.SubscribeResponse{}), nil
}

func (s *Server) UpdateStatus(
	ctx context.Context,
	req *connect.Request[devicesv1.UpdateStatusRequest],
) (*connect.Response[devicesv1.UpdateStatusResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}

	msg := &devicesv1.DeviceOperationMessage{
		DeviceId:  proto.String(req.Msg.GetId()),
		ProjectId: proto.String(principal.Project.ID),
		OperationPayload: &devicesv1.DeviceOperationMessage_UpdateStatus{
			UpdateStatus: &devicesv1.UpdateStatusPayload{
				Status: proto.String(req.Msg.GetStatus()),
			},
		},
	}

	data, err := proto.Marshal(msg)
	if err != nil {
		slog.ErrorContext(ctx, "failed to marshal device operation message", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("failed to process request"))
	}

	if _, err = s.producer.Publish(ctx, nats.DeviceOpsSubject, data); err != nil {
		slog.ErrorContext(ctx, "failed to publish device operation to NATS", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("failed to process request"))
	}

	return connect.NewResponse(&devicesv1.UpdateStatusResponse{}), nil
}

func (s *Server) UpdateToken(
	ctx context.Context,
	req *connect.Request[devicesv1.UpdateTokenRequest],
) (*connect.Response[devicesv1.UpdateTokenResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}

	msg := &devicesv1.DeviceOperationMessage{
		DeviceId:  proto.String(req.Msg.GetId()),
		ProjectId: proto.String(principal.Project.ID),
		OperationPayload: &devicesv1.DeviceOperationMessage_UpdateToken{
			UpdateToken: &devicesv1.UpdateTokenPayload{
				Token: proto.String(req.Msg.GetToken()),
			},
		},
	}

	data, err := proto.Marshal(msg)
	if err != nil {
		slog.ErrorContext(ctx, "failed to marshal device operation message", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("failed to process request"))
	}

	if _, err = s.producer.Publish(ctx, nats.DeviceOpsSubject, data); err != nil {
		slog.ErrorContext(ctx, "failed to publish device operation to NATS", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("failed to process request"))
	}

	return connect.NewResponse(&devicesv1.UpdateTokenResponse{}), nil
}
