package auth

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"
	coreauth "github.com/fivebitsio/cotton/internal/core/auth"
	"github.com/fivebitsio/cotton/internal/deps/telemetry"
	authv1 "github.com/fivebitsio/cotton/internal/gen/proto/public/auth/v1"
	"github.com/fivebitsio/cotton/internal/slogx"
	"github.com/jackc/pgx/v5/pgxpool"
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
		slog.ErrorContext(ctx, "failed to sign up", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	return connect.NewResponse(&authv1.SignUpWithEmailResponse{Token: token}), nil
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
		slog.ErrorContext(ctx, "failed to sign in", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	return connect.NewResponse(&authv1.SignInWithEmailResponse{Token: token}), nil
}
