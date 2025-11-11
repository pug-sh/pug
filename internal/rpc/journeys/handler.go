package journeys

import (
	"context"
	"log/slog"

	"connectrpc.com/connect"
	"github.com/fivebitsio/cotton/internal/core/journeys"
	journeysv1 "github.com/fivebitsio/cotton/internal/gen/proto/journeys/v1"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/fivebitsio/cotton/pkg/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/xid"
)

type server struct {
	service *journeys.Service
}

func (s *server) Create(ctx context.Context, req *connect.Request[journeysv1.CreateRequest]) (*connect.Response[journeysv1.CreateResponse], error) {
	// Note: In a real implementation, we would validate that the project belongs to the authenticated customer
	// For now, we'll trust the project ID passed in the request

	journey, err := s.service.CreateJourney(ctx, dbwrite.CreateJourneyParams{
		ID:          xid.New().String(),
		ProjectID:   req.Msg.ProjectId,
		Name:        req.Msg.Name,
		Description: postgres.StringToText(req.Msg.Description),
		State:       req.Msg.State.String(),
		EntryType:   req.Msg.EntryType.String(),
		Config:      req.Msg.Config,
		StartTime:   postgres.TimestampToTimestamptz(req.Msg.StartTime),
		EndTime:     postgres.TimestampToTimestamptz(req.Msg.EndTime),
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed creating journey", slog.Any("error", err), slog.String("projectId", req.Msg.ProjectId), slog.String("journeyName", req.Msg.Name))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	resp := connect.NewResponse(&journeysv1.CreateResponse{
		Journey: wToRPCMsg(journey),
	})

	return resp, nil
}

func (s *server) GetByProjectID(ctx context.Context, req *connect.Request[journeysv1.GetByProjectIDRequest]) (*connect.Response[journeysv1.GetByProjectIDResponse], error) {
	// Note: In a real implementation, we would validate that the project belongs to the authenticated customer
	// For now, we'll trust the project ID passed in the request

	journeys, err := s.service.GetJourneysByProjectID(ctx, req.Msg.ProjectId)
	if err != nil {
		slog.ErrorContext(ctx, "failed getting journeys by project ID", slog.Any("error", err), slog.String("projectId", req.Msg.ProjectId))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	journeyProtos := make([]*journeysv1.Journey, len(journeys))
	for i, j := range journeys {
		journeyProtos[i] = roToRPCMsg(j)
	}

	resp := connect.NewResponse(&journeysv1.GetByProjectIDResponse{
		Journeys: journeyProtos,
	})

	return resp, nil
}

func NewServer(pgRO *pgxpool.Pool, pgW *pgxpool.Pool) *server {
	service := journeys.NewService(pgRO, pgW)

	return &server{
		service: service,
	}
}
