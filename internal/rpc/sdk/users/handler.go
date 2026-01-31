package users

import (
	"context"
	"log/slog"

	"connectrpc.com/connect"
	usersv1 "github.com/fivebitsio/cotton/internal/gen/proto/users/v1"
	"github.com/fivebitsio/cotton/internal/gen/proto/users/v1/usersv1connect"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/fivebitsio/cotton/internal/rpc"
	"github.com/fivebitsio/cotton/pkg/logger/slogx"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/xid"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Handler struct {
	usersv1connect.UnimplementedUsersServiceHandler
	read  *dbread.Queries
	write *dbwrite.Queries
}

func NewHandler(pgRO *pgxpool.Pool, pgW *pgxpool.Pool) *Handler {
	return &Handler{
		read:  dbread.New(pgRO),
		write: dbwrite.New(pgW),
	}
}

func (h *Handler) Get(
	ctx context.Context,
	req *connect.Request[usersv1.GetRequest],
) (*connect.Response[usersv1.GetResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	u, err := h.read.GetUserByIDAndProjectID(ctx, dbread.GetUserByIDAndProjectIDParams{
		ID:        req.Msg.Id,
		ProjectID: principal.Project.ID,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed reading user", slogx.Error(err), slog.String("userId", req.Msg.Id))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	pbUser, err := convertUser(u)
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(&usersv1.GetResponse{
		User: pbUser,
	}), nil
}

func (h *Handler) GetByExternalId(
	ctx context.Context,
	req *connect.Request[usersv1.GetByExternalIdRequest],
) (*connect.Response[usersv1.GetByExternalIdResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	u, err := h.read.GetUserByProjectAndExternalID(ctx, dbread.GetUserByProjectAndExternalIDParams{
		ProjectID:  principal.Project.ID,
		ExternalID: req.Msg.ExternalId,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed reading user by external ID", slogx.Error(err), slog.String("externalId", req.Msg.ExternalId))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	pbUser, err := convertUser(u)
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(&usersv1.GetByExternalIdResponse{
		User: pbUser,
	}), nil
}

func (h *Handler) List(
	ctx context.Context,
	_ *connect.Request[usersv1.ListRequest],
) (*connect.Response[usersv1.ListResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	usersList, err := h.read.GetUsersByProjectID(ctx, principal.Project.ID)
	if err != nil {
		slog.ErrorContext(ctx, "failed listing users", slogx.Error(err), slog.String("projectId", principal.Project.ID))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	pbUsers := make([]*usersv1.User, len(usersList))
	for i, u := range usersList {
		pbUser, err := convertUser(u)
		if err != nil {
			return nil, err
		}
		pbUsers[i] = pbUser
	}

	return connect.NewResponse(&usersv1.ListResponse{
		Users: pbUsers,
	}), nil
}

func (h *Handler) Create(
	ctx context.Context,
	req *connect.Request[usersv1.CreateRequest],
) (*connect.Response[usersv1.CreateResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	u, err := h.write.CreateUser(ctx, dbwrite.CreateUserParams{
		ID:               xid.New().String(),
		ExternalID:       req.Msg.ExternalId,
		ProjectID:        principal.Project.ID,
		Properties:       req.Msg.Properties.AsMap(),
		CustomProperties: req.Msg.CustomProperties.AsMap(),
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed creating user", slogx.Error(err), slog.String("externalId", req.Msg.ExternalId))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	pbUser, err := convertWriteUser(u)
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(&usersv1.CreateResponse{
		User: pbUser,
	}), nil
}

func (h *Handler) UpdateProperties(
	ctx context.Context,
	req *connect.Request[usersv1.UpdatePropertiesRequest],
) (*connect.Response[usersv1.UpdatePropertiesResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	u, err := h.write.UpdateUserProperties(ctx, dbwrite.UpdateUserPropertiesParams{
		ID:         req.Msg.Id,
		ProjectID:  principal.Project.ID,
		Properties: req.Msg.Properties.AsMap(),
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed updating user properties", slogx.Error(err), slog.String("userId", req.Msg.Id))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	pbUser, err := convertWriteUser(u)
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(&usersv1.UpdatePropertiesResponse{
		User: pbUser,
	}), nil
}

func (h *Handler) UpdateCustomProperties(
	ctx context.Context,
	req *connect.Request[usersv1.UpdateCustomPropertiesRequest],
) (*connect.Response[usersv1.UpdateCustomPropertiesResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	u, err := h.write.UpdateUserCustomProperties(ctx, dbwrite.UpdateUserCustomPropertiesParams{
		ID:               req.Msg.Id,
		ProjectID:        principal.Project.ID,
		CustomProperties: req.Msg.CustomProperties.AsMap(),
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed updating user custom properties", slogx.Error(err), slog.String("userId", req.Msg.Id))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	pbUser, err := convertWriteUser(u)
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(&usersv1.UpdateCustomPropertiesResponse{
		User: pbUser,
	}), nil
}

func (h *Handler) Delete(
	ctx context.Context,
	req *connect.Request[usersv1.DeleteRequest],
) (*connect.Response[usersv1.DeleteResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	if err := h.write.DeleteUserByIDAndProjectID(ctx, dbwrite.DeleteUserByIDAndProjectIDParams{
		ID:        req.Msg.Id,
		ProjectID: principal.Project.ID,
	}); err != nil {
		slog.ErrorContext(ctx, "failed deleting user", slogx.Error(err), slog.String("userId", req.Msg.Id))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&usersv1.DeleteResponse{}), nil
}

func convertUser(u dbread.User) (*usersv1.User, error) {
	propertiesMap := u.Properties
	if propertiesMap == nil {
		propertiesMap = make(map[string]any)
	}
	properties, err := structpb.NewStruct(propertiesMap)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	customPropertiesMap := u.CustomProperties
	if customPropertiesMap == nil {
		customPropertiesMap = make(map[string]any)
	}
	customProperties, err := structpb.NewStruct(customPropertiesMap)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return &usersv1.User{
		Id:               u.ID,
		ExternalId:       u.ExternalID,
		ProjectId:        u.ProjectID,
		Properties:       properties,
		CustomProperties: customProperties,
		CreateTime:       timestamppb.New(u.CreateTime.Time),
		UpdateTime:       timestamppb.New(u.UpdateTime.Time),
	}, nil
}

func convertWriteUser(u dbwrite.User) (*usersv1.User, error) {
	propertiesMap := u.Properties
	if propertiesMap == nil {
		propertiesMap = make(map[string]any)
	}
	properties, err := structpb.NewStruct(propertiesMap)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	customPropertiesMap := u.CustomProperties
	if customPropertiesMap == nil {
		customPropertiesMap = make(map[string]any)
	}
	customProperties, err := structpb.NewStruct(customPropertiesMap)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return &usersv1.User{
		Id:               u.ID,
		ExternalId:       u.ExternalID,
		ProjectId:        u.ProjectID,
		Properties:       properties,
		CustomProperties: customProperties,
		CreateTime:       timestamppb.New(u.CreateTime.Time),
		UpdateTime:       timestamppb.New(u.UpdateTime.Time),
	}, nil
}
