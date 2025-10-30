package auth

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"
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

type Server struct {
	db     *queries
	jwtKey []byte
}

func NewServer(pgRO *pgxpool.Pool, pgW *pgxpool.Pool, jwtKey []byte) *Server {
	return &Server{
		db:     &queries{read: dbread.New(pgRO), write: dbwrite.New(pgW)},
		jwtKey: jwtKey,
	}
}

func (s *Server) SignUpWithEmail(ctx context.Context, req *connect.Request[authv1.SignUpWithEmailRequest]) (*connect.Response[authv1.SignUpWithEmailResponse], error) {
	email := req.Msg.GetEmail()
	password := req.Msg.GetPassword()

	if email == "" || password == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("email and password are required"))
	}

	return s.signUpWithEmail(ctx, email, password)
}

func (s *Server) SignInWithEmail(ctx context.Context, req *connect.Request[authv1.SignInWithEmailRequest]) (*connect.Response[authv1.SignInWithEmailResponse], error) {
	email := req.Msg.GetEmail()
	password := req.Msg.GetPassword()

	if email == "" || password == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("email and password are required"))
	}

	return s.signInWithEmail(ctx, email, password)
}

func (s *Server) signUpWithEmail(ctx context.Context, email, password string) (*connect.Response[authv1.SignUpWithEmailResponse], error) {
	_, err := s.db.read.GetCustomerByEmail(ctx, email)
	if err == nil {
		return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("customer with this email already exists"))
	}

	passwordHash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("failed to hash password"))
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
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("failed to create customer"))
	}

	token, err := generateJWT(customer.Email, s.jwtKey)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("failed to generate token"))
	}

	response := &authv1.SignUpWithEmailResponse{
		Token: token,
	}

	return connect.NewResponse(response), nil
}

func (s *Server) signInWithEmail(ctx context.Context, email, password string) (*connect.Response[authv1.SignInWithEmailResponse], error) {
	customer, err := s.db.read.GetCustomerByEmailWithPassword(ctx, email)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("invalid credentials"))
	}

	err = bcrypt.CompareHashAndPassword([]byte(customer.PasswordHash), []byte(password))
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("invalid credentials"))
	}

	token, err := generateJWT(customer.Email, s.jwtKey)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("failed to generate token"))
	}

	response := &authv1.SignInWithEmailResponse{
		Token: token,
	}

	return connect.NewResponse(response), nil
}

func generateJWT(email string, jwtKey []byte) (string, error) {
	claims := jwt.MapClaims{
		"email": email,
		"exp":   time.Now().Add(time.Hour * 24).Unix(),
		"iat":   time.Now().Unix(),
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString(jwtKey)
	if err != nil {
		return "", err
	}

	return tokenString, nil
}
