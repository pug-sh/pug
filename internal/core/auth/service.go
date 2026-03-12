package auth

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"github.com/fivebitsio/cotton/internal/core/projects"
	authv1 "github.com/fivebitsio/cotton/internal/gen/proto/auth/v1"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/xid"
	"golang.org/x/crypto/bcrypt"
)

const (
	aud = "cotton/dashboard"
	iss = "cotton/auth"
)

type Service struct {
	read            *dbread.Queries
	write           *dbwrite.Queries
	projectsService *projects.Service
	jwtKey          []byte
}

func NewService(pgRO *pgxpool.Pool, pgW *pgxpool.Pool, jwtKey []byte) *Service {
	return &Service{
		read:            dbread.New(pgRO),
		write:           dbwrite.New(pgW),
		projectsService: projects.NewService(pgRO, pgW),
		jwtKey:          jwtKey,
	}
}

func (s *Service) SignUpWithEmail(ctx context.Context, email, password string) (*authv1.SignUpWithEmailResponse, error) {
	_, err := s.read.GetCustomerByEmail(ctx, email)
	if err == nil {
		return nil, connect.NewError(connect.CodeAlreadyExists, errors.New("user with this email already exists"))
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		slog.ErrorContext(ctx, "failed to check existing customer", slog.Any("error", err))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	passwordHash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		slog.ErrorContext(ctx, "failed to hash password", slog.Any("error", err))
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	arg := dbwrite.CreateCustomerParams{
		ID:           xid.New().String(),
		Email:        email,
		DisplayName:  "",
		PictureUri:   "",
		PasswordHash: string(passwordHash),
	}

	customer, err := s.write.CreateCustomer(ctx, arg)
	if err != nil {
		slog.ErrorContext(ctx, "failed to create customer", slog.Any("error", err))
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	// Create a default project for the new customer
	_, err = s.projectsService.CreateProject(ctx, customer.ID, "default")
	if err != nil {
		slog.ErrorContext(ctx, "failed to create default project", slog.Any("error", err))
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	token, err := s.generateJWT(customer.ID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to generate JWT", slog.Any("error", err))
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	response := &authv1.SignUpWithEmailResponse{
		Token: token,
	}

	return response, nil
}

func (s *Service) SignInWithEmail(ctx context.Context, email, password string) (*authv1.SignInWithEmailResponse, error) {
	customer, err := s.read.GetCustomerByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid credentials"))
		}
		slog.ErrorContext(ctx, "failed to get customer by email", slog.Any("error", err))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	err = bcrypt.CompareHashAndPassword([]byte(customer.PasswordHash), []byte(password))
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid credentials"))
	}

	token, err := s.generateJWT(customer.ID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to generate JWT", slog.Any("error", err))
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	response := &authv1.SignInWithEmailResponse{
		Token: token,
	}

	return response, nil
}

type AdditionalClaims struct {
	Email string `json:"email"`
}

type UserClaims struct {
	jwt.RegisteredClaims
	AdditionalClaims
}

func (s *Service) generateJWT(id string) (string, error) {
	// todo - reduce expiry time
	now := time.Now()
	standardClaims := jwt.RegisteredClaims{
		Audience:  jwt.ClaimStrings{aud},
		ExpiresAt: jwt.NewNumericDate(now.Add(90 * 24 * time.Hour)), // expire in 90 days
		ID:        xid.New().String(),
		IssuedAt:  jwt.NewNumericDate(now),
		Issuer:    iss,
		Subject:   id,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, standardClaims)

	tokenString, err := token.SignedString(s.jwtKey)
	if err != nil {
		return "", err
	}

	return tokenString, nil
}
