package auth

import (
	"context"

	"connectrpc.com/connect"
	"github.com/fivebitsio/cotton/internal/core/auth"
	"github.com/fivebitsio/cotton/internal/core/orgs"
	"github.com/fivebitsio/cotton/internal/core/projects"
	authv1 "github.com/fivebitsio/cotton/internal/gen/proto/auth/v1"
	"github.com/jackc/pgx/v5/pgxpool"
)

type server struct {
	service *auth.Service
}

func NewServer(pgRO *pgxpool.Pool, pgW *pgxpool.Pool, jwtKey []byte, orgsSvc *orgs.Service, projectsSvc *projects.Service) *server {
	service := auth.NewService(pgRO, pgW, jwtKey, orgsSvc, projectsSvc)

	return &server{
		service: service,
	}
}

func (s *server) SignUpWithEmail(
	ctx context.Context,
	req *connect.Request[authv1.SignUpWithEmailRequest],
) (*connect.Response[authv1.SignUpWithEmailResponse], error) {
	response, err := s.service.SignUpWithEmail(ctx, req.Msg.GetEmail(), req.Msg.GetPassword())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(response), nil
}

func (s *server) SignInWithEmail(
	ctx context.Context,
	req *connect.Request[authv1.SignInWithEmailRequest],
) (*connect.Response[authv1.SignInWithEmailResponse], error) {
	response, err := s.service.SignInWithEmail(ctx, req.Msg.GetEmail(), req.Msg.GetPassword())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(response), nil
}
