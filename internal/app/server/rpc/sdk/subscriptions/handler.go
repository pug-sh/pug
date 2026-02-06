package subscriptions

import (
	"context"
	"log/slog"

	"connectrpc.com/connect"

	"github.com/fivebitsio/cotton/internal/app/server/rpc"
	"github.com/fivebitsio/cotton/internal/deps/nats"
	subscriptionsv1 "github.com/fivebitsio/cotton/internal/gen/proto/subscriptions/v1"
	"github.com/fivebitsio/cotton/internal/slogx"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

// Server implements the Subscriptions service
type Server struct {
	producer jetstream.JetStream
}

// Upsert adds or updates a subscription asynchronously
func (s *Server) Upsert(
	ctx context.Context,
	req *connect.Request[subscriptionsv1.UpsertRequest],
) (*connect.Response[subscriptionsv1.UpsertResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	msg := &subscriptionsv1.SubscriptionOperationMessage{
		OperationType: subscriptionsv1.SubscriptionOperationType_SUBSCRIPTION_OPERATION_TYPE_UPSERT,
		Id:            req.Msg.GetId(),
		Metadata:      req.Msg.GetMetadata(),
		Platform:      req.Msg.GetPlatform(),
		Token:         req.Msg.GetToken(),
		ProjectId:     principal.Project.ID,
	}

	data, err := proto.Marshal(msg)
	if err != nil {
		slog.ErrorContext(ctx, "failed to marshal subscription operation message", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	// Publish to NATS JetStream
	_, err = s.producer.Publish(ctx, nats.SubscriptionOpsSubject, data)
	if err != nil {
		slog.ErrorContext(ctx, "failed to publish subscription operation to NATS", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&subscriptionsv1.UpsertResponse{}), nil
}

// UpdateHeartbeat updates the last heartbeat time for a subscription asynchronously
func (s *Server) UpdateHeartbeat(
	ctx context.Context,
	req *connect.Request[subscriptionsv1.UpdateHeartbeatRequest],
) (*connect.Response[subscriptionsv1.UpdateHeartbeatResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	msg := &subscriptionsv1.SubscriptionOperationMessage{
		OperationType: subscriptionsv1.SubscriptionOperationType_SUBSCRIPTION_OPERATION_TYPE_UPDATE_HEARTBEAT,
		Id:            req.Msg.GetId(),
		ProjectId:     principal.Project.ID,
	}

	data, err := proto.Marshal(msg)
	if err != nil {
		slog.ErrorContext(ctx, "failed to marshal subscription operation message", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	// Publish to NATS JetStream
	_, err = s.producer.Publish(ctx, nats.SubscriptionOpsSubject, data)
	if err != nil {
		slog.ErrorContext(ctx, "failed to publish subscription operation to NATS", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&subscriptionsv1.UpdateHeartbeatResponse{}), nil
}

// UpdateMetadata updates the metadata for a subscription asynchronously
func (s *Server) UpdateMetadata(
	ctx context.Context,
	req *connect.Request[subscriptionsv1.UpdateMetadataRequest],
) (*connect.Response[subscriptionsv1.UpdateMetadataResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	msg := &subscriptionsv1.SubscriptionOperationMessage{
		OperationType: subscriptionsv1.SubscriptionOperationType_SUBSCRIPTION_OPERATION_TYPE_UPDATE_METADATA,
		Id:            req.Msg.GetId(),
		Metadata:      req.Msg.GetMetadata(),
		ProjectId:     principal.Project.ID,
	}

	data, err := proto.Marshal(msg)
	if err != nil {
		slog.ErrorContext(ctx, "failed to marshal subscription operation message", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	// Publish to NATS JetStream
	_, err = s.producer.Publish(ctx, nats.SubscriptionOpsSubject, data)
	if err != nil {
		slog.ErrorContext(ctx, "failed to publish subscription operation to NATS", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&subscriptionsv1.UpdateMetadataResponse{}), nil
}

// UpdateStatus updates the status of a subscription asynchronously
func (s *Server) UpdateStatus(
	ctx context.Context,
	req *connect.Request[subscriptionsv1.UpdateStatusRequest],
) (*connect.Response[subscriptionsv1.UpdateStatusResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	msg := &subscriptionsv1.SubscriptionOperationMessage{
		OperationType: subscriptionsv1.SubscriptionOperationType_SUBSCRIPTION_OPERATION_TYPE_UPDATE_STATUS,
		Id:            req.Msg.GetId(),
		Status:        req.Msg.GetStatus(),
		ProjectId:     principal.Project.ID,
	}

	data, err := proto.Marshal(msg)
	if err != nil {
		slog.ErrorContext(ctx, "failed to marshal subscription operation message", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	// Publish to NATS JetStream
	_, err = s.producer.Publish(ctx, nats.SubscriptionOpsSubject, data)
	if err != nil {
		slog.ErrorContext(ctx, "failed to publish subscription operation to NATS", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&subscriptionsv1.UpdateStatusResponse{}), nil
}

// UpdateToken updates the token for a subscription asynchronously
func (s *Server) UpdateToken(
	ctx context.Context,
	req *connect.Request[subscriptionsv1.UpdateTokenRequest],
) (*connect.Response[subscriptionsv1.UpdateTokenResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	msg := &subscriptionsv1.SubscriptionOperationMessage{
		OperationType: subscriptionsv1.SubscriptionOperationType_SUBSCRIPTION_OPERATION_TYPE_UPDATE_TOKEN,
		Id:            req.Msg.GetId(),
		Token:         req.Msg.GetToken(),
		ProjectId:     principal.Project.ID,
	}

	data, err := proto.Marshal(msg)
	if err != nil {
		slog.ErrorContext(ctx, "failed to marshal subscription operation message", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	// Publish to NATS JetStream
	_, err = s.producer.Publish(ctx, nats.SubscriptionOpsSubject, data)
	if err != nil {
		slog.ErrorContext(ctx, "failed to publish subscription operation to NATS", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&subscriptionsv1.UpdateTokenResponse{}), nil
}

// RegisterSubscription handles subscription registration (called when Pushpa.init("api_key") is called)
func (s *Server) RegisterSubscription(
	ctx context.Context,
	req *connect.Request[subscriptionsv1.RegisterSubscriptionRequest],
) (*connect.Response[subscriptionsv1.RegisterSubscriptionResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	// Create subscription operation message using upsert operation type
	msg := &subscriptionsv1.SubscriptionOperationMessage{
		OperationType: subscriptionsv1.SubscriptionOperationType_SUBSCRIPTION_OPERATION_TYPE_UPSERT,
		Id:            req.Msg.GetSubscriptionId(),
		Metadata:      req.Msg.GetMetadata(),
		Platform:      req.Msg.GetPlatform(),
		Token:         req.Msg.GetToken(),
		ProjectId:     principal.Project.ID,
	}

	data, err := proto.Marshal(msg)
	if err != nil {
		slog.ErrorContext(ctx, "failed to marshal subscription operation message", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	// Publish to NATS JetStream
	_, err = s.producer.Publish(ctx, nats.SubscriptionOpsSubject, data)
	if err != nil {
		slog.ErrorContext(ctx, "failed to publish subscription operation to NATS", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&subscriptionsv1.RegisterSubscriptionResponse{
		SubscriptionId: req.Msg.GetSubscriptionId(),
		Success:        true,
	}), nil
}

// SetProfileExternalID handles profile creation and subscription linking (called when Pushpa.setExternalId("something") is called)
func (s *Server) SetProfileExternalID(
	ctx context.Context,
	req *connect.Request[subscriptionsv1.SetProfileExternalIDRequest],
) (*connect.Response[subscriptionsv1.SetProfileExternalIDResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	// Create a profile link operation message that will be processed asynchronously
	msg := &subscriptionsv1.SubscriptionOperationMessage{
		OperationType:   subscriptionsv1.SubscriptionOperationType_SUBSCRIPTION_OPERATION_TYPE_PROFILE_LINK,
		SubscriptionId:  req.Msg.GetSubscriptionId(),
		ExternalId:      req.Msg.GetExternalId(),
		ProfileMetadata: req.Msg.GetProfileMetadata(),
		ProjectId:       principal.Project.ID,
	}

	data, err := proto.Marshal(msg)
	if err != nil {
		slog.ErrorContext(ctx, "failed to marshal subscription operation message", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	// Publish to NATS JetStream
	_, err = s.producer.Publish(ctx, nats.SubscriptionOpsSubject, data)
	if err != nil {
		slog.ErrorContext(ctx, "failed to publish subscription operation to NATS", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	// Operation is processed asynchronously via NATS. Response only confirms
	// the message was enqueued — actual profile creation and subscription linking
	// happen in the subscription worker.
	return connect.NewResponse(&subscriptionsv1.SetProfileExternalIDResponse{}), nil
}

// NewServer creates a new Subscription service server
func NewServer(js jetstream.JetStream) (*Server, error) {
	return &Server{
		producer: js,
	}, nil
}
