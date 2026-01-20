package auth

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"github.com/fivebitsio/cotton/internal/core/auth"
	authv1 "github.com/fivebitsio/cotton/internal/gen/proto/auth/v1"
	"github.com/jackc/pgx/v5/pgxpool"
)

type server struct {
	service *auth.Service
}

func NewServer(pgRO *pgxpool.Pool, pgW *pgxpool.Pool, jwtKey []byte) *server {
	service := auth.NewService(pgRO, pgW, jwtKey)

	return &server{
		service: service,
	}
}

func (s *server) SignUpWithEmail(ctx context.Context, req *connect.Request[authv1.SignUpWithEmailRequest]) (*connect.Response[authv1.SignUpWithEmailResponse], error) {
	email := req.Msg.GetEmail()
	password := req.Msg.GetPassword()

	if email == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, ErrMissingEmail)
	}

	if password == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, ErrMissingPassword)
	}

	response, err := s.service.SignUpWithEmail(ctx, email, password)
	if err != nil {
		return nil, mapErrorToConnectError(err)
	}

	return connect.NewResponse(response), nil
}

func (s *server) SignInWithEmail(ctx context.Context, req *connect.Request[authv1.SignInWithEmailRequest]) (*connect.Response[authv1.SignInWithEmailResponse], error) {
	email := req.Msg.GetEmail()
	password := req.Msg.GetPassword()

	if email == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, ErrMissingEmail)
	}

	if password == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, ErrMissingPassword)
	}

	response, err := s.service.SignInWithEmail(ctx, email, password)
	if err != nil {
		return nil, mapErrorToConnectError(err)
	}

	return connect.NewResponse(response), nil
}

func mapErrorToConnectError(err error) *connect.Error {
	switch {
	case errors.Is(err, auth.ErrUserAlreadyExists):
		return connect.NewError(connect.CodeAlreadyExists, err)
	case errors.Is(err, auth.ErrInvalidCredentials):
		return connect.NewError(connect.CodeUnauthenticated, err)
	case errors.Is(err, auth.ErrCustomerCreation):
		return connect.NewError(connect.CodeInternal, err)
	default:
		return connect.NewError(connect.CodeInternal, err)
	}
}
