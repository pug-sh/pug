package campaigns

import (
	"context"
	"log/slog"

	"connectrpc.com/connect"
	"github.com/fivebitsio/cotton/internal/core/campaigns"
	"github.com/fivebitsio/cotton/internal/core/projects"
	campaignsv1 "github.com/fivebitsio/cotton/internal/gen/proto/campaigns/v1"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/fivebitsio/cotton/internal/rpc/dashboard"
	"github.com/fivebitsio/cotton/pkg/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/rs/xid"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type server struct {
	service  *campaigns.Service
	producer jetstream.JetStream
}

func (s *server) Get(
	ctx context.Context,
	req *connect.Request[campaignsv1.GetRequest],
) (*connect.Response[campaignsv1.GetResponse], error) {
	campaign, err := s.service.GetCampaignById(ctx, req.Msg.Id)
	if err != nil {
		slog.ErrorContext(ctx, "failed getting campaign", slog.Any("error", err), slog.String("campaignId", req.Msg.Id))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	resp := connect.NewResponse(&campaignsv1.GetResponse{
		Campaign: roToRPCMsg(campaign),
	})

	return resp, nil
}

func (s *server) BatchGet(
	ctx context.Context,
	req *connect.Request[campaignsv1.BatchGetRequest],
) (*connect.Response[campaignsv1.BatchGetResponse], error) {
	campaigns, err := s.service.GetCampaignsByProjectID(ctx, req.Msg.ProjectId)
	if err != nil {
		slog.ErrorContext(ctx, "failed getting campaigns by project ID", slog.Any("error", err), slog.String("projectId", req.Msg.ProjectId))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	campaignProtos := make([]*campaignsv1.Campaign, len(campaigns))
	for i, c := range campaigns {
		campaignProtos[i] = roToRPCMsg(c)
	}

	resp := connect.NewResponse(&campaignsv1.BatchGetResponse{
		Campaigns: campaignProtos,
	})

	return resp, nil
}

func (s *server) Create(
	ctx context.Context,
	req *connect.Request[campaignsv1.CreateRequest],
) (*connect.Response[campaignsv1.CreateResponse], error) {
	customer, err := dashboard.GetCustomerFromContext(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to get customer from context", slog.Any("error", err))
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	exists, err := s.service.ProjectExistsForCustomer(ctx, req.Msg.ProjectId, customer.ID)
	if !exists || err != nil {
		slog.ErrorContext(ctx, "failed to verify project ownership", slog.Any("error", err), slog.String("projectId", req.Msg.ProjectId), slog.String("customerId", customer.ID))
		return nil, connect.NewError(connect.CodeNotFound, err)
	}

	var scheduledTimeParam *timestamppb.Timestamp
	if req.Msg.ScheduledTime == nil {
		scheduledTimeParam = timestamppb.Now()
	} else {
		scheduledTimeParam = req.Msg.ScheduledTime
	}

	campaign, err := s.service.CreateCampaign(ctx, dbwrite.CreateCampaignParams{
		ID:               xid.New().String(),
		Name:             req.Msg.Name,
		ProjectID:        req.Msg.ProjectId,
		NotificationData: req.Msg.NotificationData,
		ScheduledTime:    postgres.TimestampToTimestamptz(scheduledTimeParam),
		Status:           "scheduled",
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed creating campaign", slog.Any("error", err), slog.String("projectId", req.Msg.ProjectId), slog.String("campaignName", req.Msg.Name))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	resp := connect.NewResponse(&campaignsv1.CreateResponse{
		Campaign: wToRPCMsg(campaign),
	})

	return resp, nil
}

func (s *server) Delete(
	ctx context.Context,
	req *connect.Request[campaignsv1.DeleteRequest],
) (*connect.Response[campaignsv1.DeleteResponse], error) {
	err := s.service.DeleteCampaign(ctx, req.Msg.Id, req.Msg.ProjectId)
	if err != nil {
		slog.ErrorContext(ctx, "failed deleting campaign", slog.Any("error", err), slog.String("campaignId", req.Msg.Id))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	resp := connect.NewResponse(&campaignsv1.DeleteResponse{})

	return resp, nil
}

func (s *server) Update(
	ctx context.Context,
	req *connect.Request[campaignsv1.UpdateRequest],
) (*connect.Response[campaignsv1.UpdateResponse], error) {
	var scheduledTimeParam *timestamppb.Timestamp
	if req.Msg.ScheduledTime == nil {
		scheduledTimeParam = timestamppb.Now()
	} else {
		scheduledTimeParam = req.Msg.ScheduledTime
	}

	campaign, err := s.service.UpdateCampaign(ctx, dbwrite.UpdateCampaignParams{
		ID:               req.Msg.Id,
		Name:             req.Msg.Name,
		NotificationData: req.Msg.NotificationData,
		ScheduledTime:    postgres.TimestampToTimestamptz(scheduledTimeParam),
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed updating campaign", slog.Any("error", err), slog.String("campaignId", req.Msg.Id))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	resp := connect.NewResponse(&campaignsv1.UpdateResponse{
		Campaign: wToRPCMsg(campaign),
	})

	return resp, nil
}

func NewServer(pgRO *pgxpool.Pool, pgW *pgxpool.Pool, producer jetstream.JetStream) *server {
	projectsSvc := projects.NewService(pgRO, pgW)
	service := campaigns.NewService(pgRO, pgW, projectsSvc, producer)

	return &server{
		service:  service,
		producer: producer,
	}
}
