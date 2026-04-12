package campaigns

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"connectrpc.com/connect"
	"github.com/fivebitsio/cotton/internal/app/server/rpc"
	"github.com/fivebitsio/cotton/internal/core/campaigns"
	"github.com/fivebitsio/cotton/internal/core/projects"
	"github.com/fivebitsio/cotton/internal/deps/postgres"
	"github.com/fivebitsio/cotton/internal/deps/telemetry"
	campaignsv1 "github.com/fivebitsio/cotton/internal/gen/proto/shared/campaigns/v1"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/fivebitsio/cotton/internal/slogx"
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
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}

	campaign, err := s.service.GetCampaignByIDAndProjectID(ctx, req.Msg.Id, principal.Project.ID)
	if err != nil {
		slog.ErrorContext(ctx, "failed getting campaign", slogx.Error(err), slog.String("campaignId", req.Msg.Id))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	campaignProto, err := roToRPCMsg(campaign)
	if err != nil {
		slog.ErrorContext(ctx, "failed to convert campaign to proto", slogx.Error(err), slog.String("campaignId", req.Msg.Id))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(&campaignsv1.GetResponse{
		Campaign: campaignProto,
	}), nil
}

func (s *server) BatchGet(
	ctx context.Context,
	req *connect.Request[campaignsv1.BatchGetRequest],
) (*connect.Response[campaignsv1.BatchGetResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}

	projectID := principal.Project.ID

	campaigns, err := s.service.GetCampaignsByProjectID(ctx, projectID)
	if err != nil {
		slog.ErrorContext(ctx, "failed getting campaigns by project ID", slogx.Error(err), slog.String("projectId", projectID))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	campaignProtos := make([]*campaignsv1.Campaign, len(campaigns))
	for i, c := range campaigns {
		proto, err := roToRPCMsg(c)
		if err != nil {
			slog.ErrorContext(ctx, "failed to convert campaign to proto", slogx.Error(err), slog.String("campaignId", c.ID))
			telemetry.RecordError(ctx, err)
			return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
		}
		campaignProtos[i] = proto
	}

	return connect.NewResponse(&campaignsv1.BatchGetResponse{
		Campaigns: campaignProtos,
	}), nil
}

func (s *server) Create(
	ctx context.Context,
	req *connect.Request[campaignsv1.CreateRequest],
) (*connect.Response[campaignsv1.CreateResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}

	projectID := principal.Project.ID

	var scheduledTimeParam *timestamppb.Timestamp
	if req.Msg.ScheduledTime == nil {
		scheduledTimeParam = timestamppb.Now()
	} else {
		scheduledTimeParam = req.Msg.ScheduledTime
	}

	var notificationData map[string]any
	if err := json.Unmarshal(req.Msg.NotificationData, &notificationData); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	campaign, err := s.service.CreateCampaign(ctx, dbwrite.CreateCampaignParams{
		ID:               xid.New().String(),
		Name:             req.Msg.Name,
		ProjectID:        projectID,
		NotificationData: notificationData,
		ScheduledTime:    postgres.NewTimestamptz(scheduledTimeParam.AsTime()),
		Status:           campaigns.StatusScheduled,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed creating campaign", slogx.Error(err), slog.String("projectId", projectID), slog.String("campaignName", req.Msg.Name))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	campaignProto, err := wToRPCMsg(campaign)
	if err != nil {
		slog.ErrorContext(ctx, "failed to convert campaign to proto", slogx.Error(err), slog.String("campaignId", campaign.ID))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(&campaignsv1.CreateResponse{
		Campaign: campaignProto,
	}), nil
}

func (s *server) Delete(
	ctx context.Context,
	req *connect.Request[campaignsv1.DeleteRequest],
) (*connect.Response[campaignsv1.DeleteResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}

	projectID := principal.Project.ID

	err = s.service.DeleteCampaign(ctx, req.Msg.Id, projectID)
	if err != nil {
		slog.ErrorContext(ctx, "failed deleting campaign", slogx.Error(err), slog.String("campaignId", req.Msg.Id))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	resp := connect.NewResponse(&campaignsv1.DeleteResponse{})

	return resp, nil
}

func (s *server) Update(
	ctx context.Context,
	req *connect.Request[campaignsv1.UpdateRequest],
) (*connect.Response[campaignsv1.UpdateResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}

	// SQL uses COALESCE to preserve existing values for empty/null fields
	campaign, err := s.service.UpdateCampaign(ctx, dbwrite.UpdateCampaignParams{
		ID:               req.Msg.Id,
		ProjectID:        principal.Project.ID,
		Name:             req.Msg.Name,
		NotificationData: req.Msg.NotificationData,
		ScheduledTime:    postgres.TimestampToTimestamptz(req.Msg.ScheduledTime),
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed updating campaign", slogx.Error(err), slog.String("campaignId", req.Msg.Id))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	campaignProto, err := wToRPCMsg(campaign)
	if err != nil {
		slog.ErrorContext(ctx, "failed to convert campaign to proto", slogx.Error(err), slog.String("campaignId", campaign.ID))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(&campaignsv1.UpdateResponse{
		Campaign: campaignProto,
	}), nil
}

func NewServer(pgRO *pgxpool.Pool, pgW *pgxpool.Pool, producer jetstream.JetStream) *server {
	projectsSvc := projects.NewService(pgRO, pgW, nil)
	service := campaigns.NewService(pgRO, pgW, projectsSvc, producer)

	return &server{
		service:  service,
		producer: producer,
	}
}
