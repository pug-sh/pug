package profiles

import (
	"context"
	"log/slog"

	"connectrpc.com/connect"
	"github.com/fivebitsio/cotton/internal/app/server/rpc"
	profilesv1 "github.com/fivebitsio/cotton/internal/gen/proto/profiles/v1"
	"github.com/fivebitsio/cotton/internal/gen/proto/profiles/v1/profilesv1connect"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/fivebitsio/cotton/internal/slogx"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/xid"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Handler struct {
	profilesv1connect.UnimplementedProfilesServiceHandler
	read  *dbread.Queries
	write *dbwrite.Queries
}

func NewHandler(pgRO *pgxpool.Pool, pgW *pgxpool.Pool) *Handler {
	return &Handler{
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
		return nil, connect.NewError(connect.CodeInternal, err)
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
		return nil, connect.NewError(connect.CodeInternal, err)
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
		return nil, connect.NewError(connect.CodeInternal, err)
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
		return nil, connect.NewError(connect.CodeInternal, err)
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
		return nil, connect.NewError(connect.CodeInternal, err)
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
