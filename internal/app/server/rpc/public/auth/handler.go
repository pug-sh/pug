package auth

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"
	coreauth "github.com/pug-sh/pug/internal/core/auth"
	authv1 "github.com/pug-sh/pug/internal/gen/proto/public/auth/v1"
)

type server struct {
	service *coreauth.Service
}

func NewServer(pgRO *pgxpool.Pool, pgW *pgxpool.Pool, jwtKey []byte) *server {
	service := coreauth.NewService(pgRO, pgW, jwtKey)

	return &server{
		service: service,
	}
}

func (s *server) SignUpWithEmail(
	ctx context.Context,
	req *connect.Request[authv1.SignUpWithEmailRequest],
) (*connect.Response[authv1.SignUpWithEmailResponse], error) {
	token, err := s.service.SignUpWithEmail(ctx, req.Msg.GetEmail(), req.Msg.GetPassword())
	if err != nil {
		if errors.Is(err, coreauth.ErrEmailAlreadyExists) {
			return nil, connect.NewError(connect.CodeAlreadyExists, errors.New("user with this email already exists"))
		}
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	return connect.NewResponse(&authv1.SignUpWithEmailResponse{Token: &token}), nil
}

func (s *server) SignInWithEmail(
	ctx context.Context,
	req *connect.Request[authv1.SignInWithEmailRequest],
) (*connect.Response[authv1.SignInWithEmailResponse], error) {
	token, err := s.service.SignInWithEmail(ctx, req.Msg.GetEmail(), req.Msg.GetPassword())
	if err != nil {
		if errors.Is(err, coreauth.ErrInvalidCredentials) {
			return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid credentials"))
		}
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	return connect.NewResponse(&authv1.SignInWithEmailResponse{Token: &token}), nil
}
