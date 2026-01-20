package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/fivebitsio/cotton/internal/core/projects"
	authv1 "github.com/fivebitsio/cotton/internal/gen/proto/auth/v1"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/xid"
	"golang.org/x/crypto/bcrypt"
)

var (
	ErrUserAlreadyExists  = errors.New("user with this email already exists")
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrCustomerCreation   = errors.New("failed to create customer")
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
		return nil, ErrUserAlreadyExists
	}

	passwordHash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
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
		return nil, fmt.Errorf("%w: %w", ErrCustomerCreation, err)
	}

	// Create a default project for the new customer
	projectParams := dbwrite.CreateProjectParams{
		ID:          xid.New().String(),
		ApiKey:      xid.New().String(),
		CustomerID:  customer.ID,
		DisplayName: "default",
	}

	_, err = s.projectsService.CreateProject(ctx, projectParams)
	if err != nil {
		return nil, fmt.Errorf("failed to create default project for customer: %w", err)
	}

	token, err := s.generateJWT(customer.Email)
	if err != nil {
		return nil, err
	}

	response := &authv1.SignUpWithEmailResponse{
		Token: token,
	}

	return response, nil
}

func (s *Service) SignInWithEmail(ctx context.Context, email, password string) (*authv1.SignInWithEmailResponse, error) {
	customer, err := s.read.GetCustomerByEmailWithPassword(ctx, email)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidCredentials, err)
	}

	err = bcrypt.CompareHashAndPassword([]byte(customer.PasswordHash), []byte(password))
	if err != nil {
		return nil, ErrInvalidCredentials
	}

	token, err := s.generateJWT(customer.Email)
	if err != nil {
		return nil, err
	}

	response := &authv1.SignInWithEmailResponse{
		Token: token,
	}

	return response, nil
}

func (s *Service) generateJWT(email string) (string, error) {
	claims := jwt.MapClaims{
		"email": email,
		"exp":   time.Now().Add(time.Hour * 24).Unix(),
		"iat":   time.Now().Unix(),
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString(s.jwtKey)
	if err != nil {
		return "", err
	}

	return tokenString, nil
}
