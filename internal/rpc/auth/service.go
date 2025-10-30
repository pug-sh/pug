package auth

import (
	"context"
	"time"

	authv1 "github.com/fivebitsio/cotton/internal/gen/proto/auth/v1"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/fivebitsio/cotton/pkg/postgres"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/xid"
	"golang.org/x/crypto/bcrypt"
)

type queries struct {
	read  *dbread.Queries
	write *dbwrite.Queries
}

type Service struct {
	db     *queries
	jwtKey []byte
}

func newService(pgRO *pgxpool.Pool, pgW *pgxpool.Pool, jwtKey []byte) *Service {
	return &Service{
		db:     &queries{read: dbread.New(pgRO), write: dbwrite.New(pgW)},
		jwtKey: jwtKey,
	}
}

func (s *Service) SignUpWithEmail(ctx context.Context, email, password string) (*authv1.SignUpWithEmailResponse, error) {
	_, err := s.db.read.GetCustomerByEmail(ctx, email)
	if err == nil {
		return nil, ErrUserAlreadyExists
	}

	passwordHash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}

	customerID := xid.New().String()
	arg := dbwrite.CreateCustomerParams{
		ID:           customerID,
		Email:        email,
		DisplayName:  postgres.StringToText(""),
		PictureUri:   postgres.StringToText(""),
		PasswordHash: string(passwordHash),
	}

	customer, err := s.db.write.CreateCustomer(ctx, arg)
	if err != nil {
		return nil, ErrCustomerCreation
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
	customer, err := s.db.read.GetCustomerByEmailWithPassword(ctx, email)
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
