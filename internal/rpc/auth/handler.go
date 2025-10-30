package auth

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	authv1 "github.com/fivebitsio/cotton/internal/gen/proto/auth/v1"
	"github.com/jackc/pgx/v5/pgxpool"
)

type server struct {
	service *Service
}

func NewServer(pgRO *pgxpool.Pool, pgW *pgxpool.Pool, jwtKey []byte) *server {
	service := newService(pgRO, pgW, jwtKey)

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
	case errors.Is(err, ErrUserAlreadyExists):
		return connect.NewError(connect.CodeAlreadyExists, err)
	case errors.Is(err, ErrInvalidCredentials):
		return connect.NewError(connect.CodeUnauthenticated, err)
	case errors.Is(err, ErrCustomerCreation):
		return connect.NewError(connect.CodeInternal, err)
	default:
		return connect.NewError(connect.CodeInternal, err)
	}
}
