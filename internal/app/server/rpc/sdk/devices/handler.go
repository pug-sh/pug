package devices

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"

	"github.com/fivebitsio/cotton/internal/app/server/rpc"
	"github.com/fivebitsio/cotton/internal/deps/nats"
	devicesv1 "github.com/fivebitsio/cotton/internal/gen/proto/devices/v1"
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
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	msg := &devicesv1.DeviceOperationMessage{
		OperationType:     devicesv1.DeviceOperationType_DEVICE_OPERATION_TYPE_SUBSCRIBE,
		DeviceId:          req.Msg.GetDeviceId(),
		Platform:          req.Msg.GetPlatform(),
		ProfileExternalId: req.Msg.GetProfileExternalId(),
		ProfileId:         req.Msg.GetProfileId(),
		Properties:        req.Msg.GetProperties(),
		Token:             req.Msg.GetToken(),
		ProjectId:         principal.Project.ID,
	}

	data, err := proto.Marshal(msg)
	if err != nil {
		slog.ErrorContext(ctx, "failed to marshal subscribe message", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, errors.New("failed to process request"))
	}

	if _, err = s.producer.Publish(ctx, nats.DeviceOpsSubject, data); err != nil {
		slog.ErrorContext(ctx, "failed to publish subscribe operation to NATS", slogx.Error(err))
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
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	msg := &devicesv1.DeviceOperationMessage{
		OperationType: devicesv1.DeviceOperationType_DEVICE_OPERATION_TYPE_UPDATE_STATUS,
		DeviceId:      req.Msg.GetId(),
		Status:        req.Msg.GetStatus(),
		ProjectId:     principal.Project.ID,
	}

	data, err := proto.Marshal(msg)
	if err != nil {
		slog.ErrorContext(ctx, "failed to marshal device operation message", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, errors.New("failed to process request"))
	}

	if _, err = s.producer.Publish(ctx, nats.DeviceOpsSubject, data); err != nil {
		slog.ErrorContext(ctx, "failed to publish device operation to NATS", slogx.Error(err))
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
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	msg := &devicesv1.DeviceOperationMessage{
		OperationType: devicesv1.DeviceOperationType_DEVICE_OPERATION_TYPE_UPDATE_TOKEN,
		DeviceId:      req.Msg.GetId(),
		Token:         req.Msg.GetToken(),
		ProjectId:     principal.Project.ID,
	}

	data, err := proto.Marshal(msg)
	if err != nil {
		slog.ErrorContext(ctx, "failed to marshal device operation message", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, errors.New("failed to process request"))
	}

	if _, err = s.producer.Publish(ctx, nats.DeviceOpsSubject, data); err != nil {
		slog.ErrorContext(ctx, "failed to publish device operation to NATS", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, errors.New("failed to process request"))
	}

	return connect.NewResponse(&devicesv1.UpdateTokenResponse{}), nil
}
