package profiles

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"connectrpc.com/connect"
	"github.com/fivebitsio/cotton/internal/app/server/rpc"
	profilesv1 "github.com/fivebitsio/cotton/internal/gen/proto/profiles/v1"
	"github.com/fivebitsio/cotton/internal/gen/proto/profiles/v1/profilesv1connect"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/deps/postgres"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/fivebitsio/cotton/internal/slogx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/xid"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Handler struct {
	profilesv1connect.UnimplementedProfilesServiceHandler
	pgW   *pgxpool.Pool
	read  *dbread.Queries
	write *dbwrite.Queries
}

func NewHandler(pgRO *pgxpool.Pool, pgW *pgxpool.Pool) *Handler {
	return &Handler{
		pgW:   pgW,
		read:  dbread.New(pgRO),
		write: dbwrite.New(pgW),
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

func (h *Handler) Merge(
	ctx context.Context,
	req *connect.Request[profilesv1.MergeRequest],
) (*connect.Response[profilesv1.MergeResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	if req.Msg.SourceId == req.Msg.TargetId {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("source and target must be different profiles"))
	}

	tx, err := h.pgW.Begin(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed starting transaction", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("failed to merge profiles"))
	}
	defer tx.Rollback(ctx)

	qtx := h.write.WithTx(tx)

	p, err := qtx.MergeProfileProperties(ctx, dbwrite.MergeProfilePropertiesParams{
		SourceID:  req.Msg.SourceId,
		TargetID:  req.Msg.TargetId,
		ProjectID: principal.Project.ID,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed merging profile properties", slogx.Error(err),
			slog.String("sourceId", req.Msg.SourceId), slog.String("targetId", req.Msg.TargetId))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("source or target profile not found"))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("failed to merge profiles"))
	}

	if err := qtx.ReassignProfileSubscriptions(ctx, dbwrite.ReassignProfileSubscriptionsParams{
		TargetID: postgres.NewText(req.Msg.TargetId),
		SourceID: postgres.NewText(req.Msg.SourceId),
	}); err != nil {
		slog.ErrorContext(ctx, "failed reassigning subscriptions", slogx.Error(err),
			slog.String("sourceId", req.Msg.SourceId), slog.String("targetId", req.Msg.TargetId))
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("failed to merge profiles"))
	}

	if err := qtx.DeleteProfileByIDAndProjectID(ctx, dbwrite.DeleteProfileByIDAndProjectIDParams{
		ID:        req.Msg.SourceId,
		ProjectID: principal.Project.ID,
	}); err != nil {
		slog.ErrorContext(ctx, "failed deleting source profile", slogx.Error(err),
			slog.String("sourceId", req.Msg.SourceId))
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("failed to merge profiles"))
	}

	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "failed committing merge transaction", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("failed to merge profiles"))
	}

	pbProfile, err := convertWriteProfile(p)
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(&profilesv1.MergeResponse{
		Profile: pbProfile,
	}), nil
}

func (h *Handler) Save(
	ctx context.Context,
	req *connect.Request[profilesv1.SaveRequest],
) (*connect.Response[profilesv1.SaveResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	p, err := h.write.SaveProfile(ctx, dbwrite.SaveProfileParams{
		AutoProperties:   req.Msg.AutoProperties.AsMap(),
		CustomProperties: req.Msg.CustomProperties.AsMap(),
		ExternalID:       req.Msg.ExternalId,
		ID:               xid.New().String(),
		ProjectID:        principal.Project.ID,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed saving profile", slogx.Error(err), slog.String("externalId", req.Msg.ExternalId))
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("failed to save profile"))
	}

	pbProfile, err := convertWriteProfile(p)
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(&profilesv1.SaveResponse{
		Profile: pbProfile,
	}), nil
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
