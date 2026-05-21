package auth

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pug-sh/pug/internal/apperr"
	coreauth "github.com/pug-sh/pug/internal/core/auth"
	natsdeps "github.com/pug-sh/pug/internal/deps/nats"
	authv1 "github.com/pug-sh/pug/internal/gen/proto/public/auth/v1"
)

// authService is the coreauth.Service surface the handler depends on, defined
// consumer-side so handlers can be unit-tested with a fake (instead of
// re-implementing the mapping logic in tests).
type authService interface {
	SignInWithEmail(ctx context.Context, email, password string) (string, error)
	RequestMagicLink(ctx context.Context, email string) error
	CompleteMagicLink(ctx context.Context, token string) (string, error)
}

type server struct {
	service authService
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
			return nil, apperr.Unauthenticated(apperr.ReasonInvalidCredentials, "invalid credentials")
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
			return nil, apperr.Invalid(apperr.ReasonInvalidToken, "invalid or expired link")
		}
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	return connect.NewResponse(&authv1.CompleteMagicLinkResponse{Token: &token}), nil
}
