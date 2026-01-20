package users

import (
	"context"
	"encoding/json"

	"connectrpc.com/connect"
	usersv1 "github.com/fivebitsio/cotton/internal/gen/proto/users/v1"
	"github.com/fivebitsio/cotton/internal/gen/proto/users/v1/usersv1connect"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/jackc/pgx/v5/pgxpool"
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
	u, err := h.read.GetUserByID(ctx, req.Msg.Id)
	if err != nil {
		return nil, err
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
	u, err := h.read.GetUserByProjectAndExternalID(ctx, dbread.GetUserByProjectAndExternalIDParams{
		ProjectID:  req.Msg.ProjectId,
		ExternalID: req.Msg.ExternalId,
	})
	if err != nil {
		return nil, err
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
	req *connect.Request[usersv1.ListRequest],
) (*connect.Response[usersv1.ListResponse], error) {
	usersList, err := h.read.GetUsersByProjectID(ctx, req.Msg.ProjectId)
	if err != nil {
		return nil, err
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
	props := map[string]any{}
	if req.Msg.Properties != nil {
		props = req.Msg.Properties.AsMap()
	}
	propsBytes, err := json.Marshal(props)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	customProps := map[string]any{}
	if req.Msg.CustomProperties != nil {
		customProps = req.Msg.CustomProperties.AsMap()
	}
	customPropsBytes, err := json.Marshal(customProps)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	u, err := h.write.CreateUser(ctx, dbwrite.CreateUserParams{
		ExternalID:       req.Msg.ExternalId,
		ProjectID:        req.Msg.ProjectId,
		Properties:       propsBytes,
		CustomProperties: customPropsBytes,
	})
	if err != nil {
		return nil, err
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
	var props map[string]any
	if req.Msg.Properties != nil {
		props = req.Msg.Properties.AsMap()
	}
	propsBytes, err := json.Marshal(props)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	u, err := h.write.UpdateUserProperties(ctx, dbwrite.UpdateUserPropertiesParams{
		ID:         req.Msg.Id,
		Properties: propsBytes,
	})
	if err != nil {
		return nil, err
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
	var customProps map[string]any
	if req.Msg.CustomProperties != nil {
		customProps = req.Msg.CustomProperties.AsMap()
	}
	customPropsBytes, err := json.Marshal(customProps)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	u, err := h.write.UpdateUserCustomProperties(ctx, dbwrite.UpdateUserCustomPropertiesParams{
		ID:               req.Msg.Id,
		CustomProperties: customPropsBytes,
	})
	if err != nil {
		return nil, err
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
	err := h.write.DeleteUserByID(ctx, req.Msg.Id)
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(&usersv1.DeleteResponse{}), nil
}

func convertUser(u dbread.User) (*usersv1.User, error) {
	propertiesMap := make(map[string]any)
	if len(u.Properties) > 0 {
		if err := json.Unmarshal(u.Properties, &propertiesMap); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
	}
	properties, err := structpb.NewStruct(propertiesMap)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	customPropertiesMap := make(map[string]any)
	if len(u.CustomProperties) > 0 {
		if err := json.Unmarshal(u.CustomProperties, &customPropertiesMap); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
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
	// dbwrite.User properties are []byte
	propertiesMap := make(map[string]any)
	if len(u.Properties) > 0 {
		if err := json.Unmarshal(u.Properties, &propertiesMap); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
	}

	properties, err := structpb.NewStruct(propertiesMap)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	customPropertiesMap := make(map[string]any)
	if len(u.CustomProperties) > 0 {
		if err := json.Unmarshal(u.CustomProperties, &customPropertiesMap); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
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
