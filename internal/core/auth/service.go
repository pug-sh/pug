package auth

import (
	"context"
	"errors"
	"time"

	"github.com/fivebitsio/cotton/internal/core/projects"
	authv1 "github.com/fivebitsio/cotton/internal/gen/proto/auth/v1"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/fivebitsio/cotton/pkg/postgres"
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
	repo            Repo
	projectsService *projects.Service
	jwtKey          []byte
}

func NewService(pgRO *pgxpool.Pool, pgW *pgxpool.Pool, jwtKey []byte) *Service {
	return &Service{
		repo:            NewRepo(pgRO, pgW),
		projectsService: projects.NewService(pgRO, pgW),
		jwtKey:          jwtKey,
	}
}

func (s *Service) SignUpWithEmail(ctx context.Context, email, password string) (*authv1.SignUpWithEmailResponse, error) {
	_, err := s.repo.GetCustomerByEmail(ctx, email)
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
		DisplayName:  postgres.StringToText(""),
		PictureUri:   postgres.StringToText(""),
		PasswordHash: string(passwordHash),
	}

	customer, err := s.repo.CreateCustomer(ctx, arg)
	if err != nil {
		return nil, ErrCustomerCreation
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
		return nil, err
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
	customer, err := s.repo.GetCustomerByEmailWithPassword(ctx, email)
	if err != nil {
		return nil, ErrInvalidCredentials
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
