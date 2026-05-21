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

func (s *server) SignUpWithEmail(
	ctx context.Context,
	req *connect.Request[authv1.SignUpWithEmailRequest],
) (*connect.Response[authv1.SignUpWithEmailResponse], error) {
	token, err := s.service.SignUpWithEmail(ctx, req.Msg.GetEmail(), req.Msg.GetPassword())
	if err != nil {
		if errors.Is(err, coreauth.ErrEmailAlreadyExists) {
			return nil, connect.NewError(connect.CodeAlreadyExists, errors.New("user with this email already exists"))
		}
		if errors.Is(err, coreauth.ErrPasswordTooLong) {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("password must be 72 bytes or fewer"))
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

func (s *server) VerifyEmail(
	ctx context.Context,
	req *connect.Request[authv1.VerifyEmailRequest],
) (*connect.Response[authv1.VerifyEmailResponse], error) {
	if err := s.service.VerifyEmail(ctx, req.Msg.GetToken()); err != nil {
		if errors.Is(err, coreauth.ErrInvalidToken) {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid or expired token"))
		}
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	return connect.NewResponse(&authv1.VerifyEmailResponse{}), nil
}

func (s *server) RequestPasswordReset(
	ctx context.Context,
	req *connect.Request[authv1.RequestPasswordResetRequest],
) (*connect.Response[authv1.RequestPasswordResetResponse], error) {
	if err := s.service.RequestPasswordReset(ctx, req.Msg.GetEmail()); err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	return connect.NewResponse(&authv1.RequestPasswordResetResponse{}), nil
}

func (s *server) ResetPassword(
	ctx context.Context,
	req *connect.Request[authv1.ResetPasswordRequest],
) (*connect.Response[authv1.ResetPasswordResponse], error) {
	if err := s.service.ResetPassword(ctx, req.Msg.GetToken(), req.Msg.GetPassword()); err != nil {
		if errors.Is(err, coreauth.ErrInvalidToken) {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid or expired token"))
		}
		if errors.Is(err, coreauth.ErrPasswordTooLong) {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("password must be 72 bytes or fewer"))
		}
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	return connect.NewResponse(&authv1.ResetPasswordResponse{}), nil
}

func (s *server) ResendVerificationEmail(
	ctx context.Context,
	req *connect.Request[authv1.ResendVerificationEmailRequest],
) (*connect.Response[authv1.ResendVerificationEmailResponse], error) {
	if err := s.service.ResendVerificationEmail(ctx, req.Msg.GetEmail()); err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	return connect.NewResponse(&authv1.ResendVerificationEmailResponse{}), nil
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
