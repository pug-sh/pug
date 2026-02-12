package profiles

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"connectrpc.com/connect"
	"github.com/fivebitsio/cotton/internal/app/server/rpc"
	"github.com/fivebitsio/cotton/internal/deps/nats"
	devicesv1 "github.com/fivebitsio/cotton/internal/gen/proto/devices/v1"
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
	pgW      *pgxpool.Pool
	producer jetstream.JetStream
	read     *dbread.Queries
	write    *dbwrite.Queries
}

func NewHandler(pgRO *pgxpool.Pool, pgW *pgxpool.Pool, js jetstream.JetStream) *Handler {
	return &Handler{
		pgW:      pgW,
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

	projectID := principal.Project.ID

	tx, err := h.pgW.Begin(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed starting transaction", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("failed to identify profile"))
	}
	defer func() {
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			slog.ErrorContext(ctx, "failed rolling back transaction", slogx.Error(err))
		}
	}()

	qtx := h.write.WithTx(tx)

	existing, err := qtx.GetProfileByProjectAndExternalID(ctx, dbwrite.GetProfileByProjectAndExternalIDParams{
		ProjectID:  projectID,
		ExternalID: req.Msg.ExternalId,
	})

	var p dbwrite.Profile

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// No profile with this external_id — assign it to the anonymous profile.
		p, err = qtx.SetProfileExternalID(ctx, dbwrite.SetProfileExternalIDParams{
			ExternalID: req.Msg.ExternalId,
			ID:         req.Msg.ProfileId,
			ProjectID:  projectID,
		})
		if err != nil {
			slog.ErrorContext(ctx, "failed setting external ID on profile", slogx.Error(err),
				slog.String("profileId", req.Msg.ProfileId), slog.String("externalId", req.Msg.ExternalId))
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("profile not found"))
			}
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("failed to identify profile"))
		}

	case err != nil:
		slog.ErrorContext(ctx, "failed looking up profile by external ID", slogx.Error(err),
			slog.String("externalId", req.Msg.ExternalId))
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("failed to identify profile"))

	case existing.ID == req.Msg.ProfileId:
		// Already identified — no-op.
		p = existing

	default:
		// Different profile owns this external_id — merge anonymous into existing.
		p, err = qtx.MergeProfileProperties(ctx, dbwrite.MergeProfilePropertiesParams{
			SourceID:  req.Msg.ProfileId,
			TargetID:  existing.ID,
			ProjectID: projectID,
		})
		if err != nil {
			slog.ErrorContext(ctx, "failed merging profile properties", slogx.Error(err),
				slog.String("sourceId", req.Msg.ProfileId), slog.String("targetId", existing.ID))
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("profile not found"))
			}
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("failed to identify profile"))
		}

		if err := qtx.ReassignProfileDevices(ctx, dbwrite.ReassignProfileDevicesParams{
			TargetID:  existing.ID,
			SourceID:  req.Msg.ProfileId,
			ProjectID: projectID,
		}); err != nil {
			slog.ErrorContext(ctx, "failed reassigning devices", slogx.Error(err),
				slog.String("sourceId", req.Msg.ProfileId), slog.String("targetId", existing.ID))
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("failed to identify profile"))
		}

		if err := qtx.DeleteProfileByIDAndProjectID(ctx, dbwrite.DeleteProfileByIDAndProjectIDParams{
			ID:        req.Msg.ProfileId,
			ProjectID: projectID,
		}); err != nil {
			slog.ErrorContext(ctx, "failed deleting source profile", slogx.Error(err),
				slog.String("sourceId", req.Msg.ProfileId))
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("failed to identify profile"))
		}
	}

	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "failed committing identify transaction", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("failed to identify profile"))
	}

	pbProfile, err := convertWriteProfile(p)
	if err != nil {
		slog.ErrorContext(ctx, "failed converting profile", slogx.Error(err),
			slog.String("profileId", p.ID))
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("failed to identify profile"))
	}

	return connect.NewResponse(&profilesv1.IdentifyResponse{
		Profile: pbProfile,
	}), nil
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
		ExternalId:       req.Msg.GetExternalId(),
		ProjectId:        principal.Project.ID,
	}

	data, err := proto.Marshal(msg)
	if err != nil {
		slog.ErrorContext(ctx, "failed to marshal profile operation message", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	if _, err = h.producer.Publish(ctx, nats.ProfileOpsSubject, data); err != nil {
		slog.ErrorContext(ctx, "failed to publish profile operation to NATS", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&profilesv1.RegisterResponse{}), nil
}

func (h *Handler) Subscribe(
	ctx context.Context,
	req *connect.Request[profilesv1.SubscribeRequest],
) (*connect.Response[profilesv1.SubscribeResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	msg := &devicesv1.DeviceOperationMessage{
		OperationType:     devicesv1.DeviceOperationType_DEVICE_OPERATION_TYPE_UPSERT,
		DeviceId:          req.Msg.GetDeviceId(),
		Platform:          req.Msg.GetPlatform(),
		ProfileExternalId: req.Msg.GetProfileExternalId(),
		ProfileId:         req.Msg.GetProfileId(),
		Token:             req.Msg.GetToken(),
		ProjectId:         principal.Project.ID,
	}

	data, err := proto.Marshal(msg)
	if err != nil {
		slog.ErrorContext(ctx, "failed to marshal subscribe message", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	if _, err = h.producer.Publish(ctx, nats.DeviceOpsSubject, data); err != nil {
		slog.ErrorContext(ctx, "failed to publish subscribe operation to NATS", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&profilesv1.SubscribeResponse{}), nil
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
		ExternalId:       p.ExternalID,
		Id:               p.ID,
		ProjectId:        p.ProjectID,
		UpdateTime:       timestamppb.New(p.UpdateTime.Time),
	}, nil
}

func convertWriteProfile(p dbwrite.Profile) (*profilesv1.Profile, error) {
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
		ExternalId:       p.ExternalID,
		Id:               p.ID,
		ProjectId:        p.ProjectID,
		UpdateTime:       timestamppb.New(p.UpdateTime.Time),
	}, nil
}
