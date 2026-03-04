package profiles

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"connectrpc.com/connect"
	"github.com/fivebitsio/cotton/internal/app/server/rpc"
	"github.com/fivebitsio/cotton/internal/deps/nats"
	profilesv1 "github.com/fivebitsio/cotton/internal/gen/proto/profiles/v1"
	"github.com/fivebitsio/cotton/internal/gen/proto/profiles/v1/profilesv1connect"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/fivebitsio/cotton/internal/slogx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Handler struct {
	profilesv1connect.UnimplementedProfilesServiceHandler
	producer jetstream.JetStream
	read     *dbread.Queries
	write    *dbwrite.Queries
}

func NewHandler(pgRO *pgxpool.Pool, pgW *pgxpool.Pool, js jetstream.JetStream) *Handler {
	return &Handler{
		producer: js,
		read:     dbread.New(pgRO),
		write:    dbwrite.New(pgW),
	}
}

func (h *Handler) Delete(
	ctx context.Context,
	req *connect.Request[profilesv1.DeleteRequest],
) (*connect.Response[profilesv1.DeleteResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	if err := h.write.DeleteProfileByIDAndProjectID(ctx, dbwrite.DeleteProfileByIDAndProjectIDParams{
		ID:        req.Msg.Id,
		ProjectID: principal.Project.ID,
	}); err != nil {
		slog.ErrorContext(ctx, "failed deleting profile", slogx.Error(err), slog.String("profileId", req.Msg.Id))
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("failed to delete profile"))
	}

	return connect.NewResponse(&profilesv1.DeleteResponse{}), nil
}

func (h *Handler) Get(
	ctx context.Context,
	req *connect.Request[profilesv1.GetRequest],
) (*connect.Response[profilesv1.GetResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	p, err := h.read.GetProfileByIDAndProjectID(ctx, dbread.GetProfileByIDAndProjectIDParams{
		ID:        req.Msg.Id,
		ProjectID: principal.Project.ID,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed reading profile", slogx.Error(err), slog.String("profileId", req.Msg.Id))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("failed to get profile"))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("failed to get profile"))
	}

	pbProfile, err := convertProfile(p)
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(&profilesv1.GetResponse{
		Profile: pbProfile,
	}), nil
}

func (h *Handler) GetByExternalId(
	ctx context.Context,
	req *connect.Request[profilesv1.GetByExternalIdRequest],
) (*connect.Response[profilesv1.GetByExternalIdResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	p, err := h.read.GetProfileByProjectAndExternalID(ctx, dbread.GetProfileByProjectAndExternalIDParams{
		ExternalID: req.Msg.ExternalId,
		ProjectID:  principal.Project.ID,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed reading profile by external ID", slogx.Error(err), slog.String("externalId", req.Msg.ExternalId))

		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("failed to get profile"))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("failed to get profile"))
	}

	pbProfile, err := convertProfile(p)
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(&profilesv1.GetByExternalIdResponse{
		Profile: pbProfile,
	}), nil
}

func (h *Handler) List(
	ctx context.Context,
	_ *connect.Request[profilesv1.ListRequest],
) (*connect.Response[profilesv1.ListResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	profilesList, err := h.read.GetProfilesByProjectID(ctx, principal.Project.ID)
	if err != nil {
		slog.ErrorContext(ctx, "failed listing profiles", slogx.Error(err), slog.String("projectId", principal.Project.ID))
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("failed to list profiles"))
	}

	pbProfiles := make([]*profilesv1.Profile, len(profilesList))
	for i, p := range profilesList {
		pbProfile, err := convertProfile(p)
		if err != nil {
			return nil, err
		}
		pbProfiles[i] = pbProfile
	}

	return connect.NewResponse(&profilesv1.ListResponse{
		Profiles: pbProfiles,
	}), nil
}

func (h *Handler) Identify(
	ctx context.Context,
	req *connect.Request[profilesv1.IdentifyRequest],
) (*connect.Response[profilesv1.IdentifyResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	msg := &profilesv1.ProfileOperationMessage{
		OperationType: profilesv1.ProfileOperationType_PROFILE_OPERATION_TYPE_IDENTIFY,
		ExternalId:    req.Msg.GetExternalId(),
		ProfileId:     req.Msg.GetProfileId(),
		ProjectId:     principal.Project.ID,
	}

	data, err := proto.Marshal(msg)
	if err != nil {
		slog.ErrorContext(ctx, "failed to marshal identify message", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, errors.New("failed to process request"))
	}

	if _, err = h.producer.Publish(ctx, nats.ProfileOpsSubject, data); err != nil {
		slog.ErrorContext(ctx, "failed to publish identify operation to NATS", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, errors.New("failed to process request"))
	}

	return connect.NewResponse(&profilesv1.IdentifyResponse{}), nil
}

func (h *Handler) Register(
	ctx context.Context,
	req *connect.Request[profilesv1.RegisterRequest],
) (*connect.Response[profilesv1.RegisterResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	msg := &profilesv1.ProfileOperationMessage{
		OperationType:    profilesv1.ProfileOperationType_PROFILE_OPERATION_TYPE_REGISTER,
		AutoProperties:   req.Msg.GetAutoProperties(),
		CustomProperties: req.Msg.GetCustomProperties(),
		ProfileId:        req.Msg.GetProfileId(),
		ProjectId:        principal.Project.ID,
	}

	data, err := proto.Marshal(msg)
	if err != nil {
		slog.ErrorContext(ctx, "failed to marshal profile operation message", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, errors.New("failed to process request"))
	}

	if _, err = h.producer.Publish(ctx, nats.ProfileOpsSubject, data); err != nil {
		slog.ErrorContext(ctx, "failed to publish profile operation to NATS", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, errors.New("failed to process request"))
	}

	return connect.NewResponse(&profilesv1.RegisterResponse{}), nil
}

func convertProfile(p dbread.Profile) (*profilesv1.Profile, error) {
	autoPropertiesMap := p.AutoProperties
	if autoPropertiesMap == nil {
		autoPropertiesMap = make(map[string]any)
	}
	autoProperties, err := structpb.NewStruct(autoPropertiesMap)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	customPropertiesMap := p.CustomProperties
	if customPropertiesMap == nil {
		customPropertiesMap = make(map[string]any)
	}
	customProperties, err := structpb.NewStruct(customPropertiesMap)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return &profilesv1.Profile{
		AutoProperties:   autoProperties,
		CreateTime:       timestamppb.New(p.CreateTime.Time),
		CustomProperties: customProperties,
		ExternalId:       p.ExternalID.String,
		Id:               p.ID,
		ProjectId:        p.ProjectID,
		UpdateTime:       timestamppb.New(p.UpdateTime.Time),
	}, nil
}
