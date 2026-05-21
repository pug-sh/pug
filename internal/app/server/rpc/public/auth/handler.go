package auth

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"
	coreauth "github.com/pug-sh/pug/internal/core/auth"
	natsdeps "github.com/pug-sh/pug/internal/deps/nats"
	authv1 "github.com/pug-sh/pug/internal/gen/proto/public/auth/v1"
)

type server struct {
	service *coreauth.Service
}

func NewServer(pgRO *pgxpool.Pool, pgW *pgxpool.Pool, jwtKey []byte, publisher *natsdeps.NATSClient) *server {
	service := coreauth.NewService(pgRO, pgW, jwtKey, publisher)

	return &server{
		service: service,
	}
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

func (s *server) RequestMagicLink(
	ctx context.Context,
	req *connect.Request[authv1.RequestMagicLinkRequest],
) (*connect.Response[authv1.RequestMagicLinkResponse], error) {
	if err := s.service.RequestMagicLink(ctx, req.Msg.GetEmail()); err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	return connect.NewResponse(&authv1.RequestMagicLinkResponse{}), nil
}

func (s *server) CompleteMagicLink(
	ctx context.Context,
	req *connect.Request[authv1.CompleteMagicLinkRequest],
) (*connect.Response[authv1.CompleteMagicLinkResponse], error) {
	token, err := s.service.CompleteMagicLink(ctx, req.Msg.GetToken())
	if err != nil {
		if errors.Is(err, coreauth.ErrInvalidToken) {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid or expired link"))
		}
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	return connect.NewResponse(&authv1.CompleteMagicLinkResponse{Token: &token}), nil
}
