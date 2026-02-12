package devices

import (
	"context"
	"log/slog"

	"connectrpc.com/connect"
	"github.com/rs/xid"

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

func NewServer(js jetstream.JetStream) (*Server, error) {
	return &Server{
		producer: js,
	}, nil
}

func (s *Server) Upsert(
	ctx context.Context,
	req *connect.Request[devicesv1.UpsertRequest],
) (*connect.Response[devicesv1.UpsertResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	deviceID := req.Msg.GetId()
	if deviceID == "" {
		deviceID = xid.New().String()
	}

	msg := &devicesv1.DeviceOperationMessage{
		OperationType:     devicesv1.DeviceOperationType_DEVICE_OPERATION_TYPE_UPSERT,
		DeviceId:          deviceID,
		Platform:          req.Msg.GetPlatform(),
		ProfileExternalId: req.Msg.GetProfileExternalId(),
		Properties:        req.Msg.GetProperties(),
		Token:             req.Msg.GetToken(),
		ProjectId:         principal.Project.ID,
	}

	data, err := proto.Marshal(msg)
	if err != nil {
		slog.ErrorContext(ctx, "failed to marshal device operation message", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	if _, err = s.producer.Publish(ctx, nats.DeviceOpsSubject, data); err != nil {
		slog.ErrorContext(ctx, "failed to publish device operation to NATS", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&devicesv1.UpsertResponse{}), nil
}

func (s *Server) UpdateStatus(
	ctx context.Context,
	req *connect.Request[devicesv1.UpdateStatusRequest],
) (*connect.Response[devicesv1.UpdateStatusResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

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
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	if _, err = s.producer.Publish(ctx, nats.DeviceOpsSubject, data); err != nil {
		slog.ErrorContext(ctx, "failed to publish device operation to NATS", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&devicesv1.UpdateStatusResponse{}), nil
}

func (s *Server) UpdateToken(
	ctx context.Context,
	req *connect.Request[devicesv1.UpdateTokenRequest],
) (*connect.Response[devicesv1.UpdateTokenResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

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
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	if _, err = s.producer.Publish(ctx, nats.DeviceOpsSubject, data); err != nil {
		slog.ErrorContext(ctx, "failed to publish device operation to NATS", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&devicesv1.UpdateTokenResponse{}), nil
}
