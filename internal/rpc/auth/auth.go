package auth

import (
	"context"
	"fmt"
	"time"

	connect "connectrpc.com/connect"
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

// Server implements the AuthService handler
type Server struct {
	db     *queries
	jwtKey []byte
}

// NewServer creates a new auth server with HMAC JWT authentication
func NewServer(pgRO *pgxpool.Pool, pgW *pgxpool.Pool, jwtKey []byte) *Server {
	return &Server{
		db:     &queries{read: dbread.New(pgRO), write: dbwrite.New(pgW)},
		jwtKey: jwtKey,
	}
}

// SignUp implements the SignUp method of AuthService
func (s *Server) SignUp(ctx context.Context, req *connect.Request[authv1.SignUpRequest]) (*connect.Response[authv1.SignUpResponse], error) {
	email := req.Msg.GetEmail()
	password := req.Msg.GetPassword()

	// Validate input
	if email == "" || password == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("email and password are required"))
	}

	_, err := s.db.read.GetCustomerByEmail(ctx, email)
	if err == nil {
		// Customer already exists
		return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("customer with this email already exists"))
	}

	// Hash the password
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

	// Generate JWT token
	token, err := s.generateJWT(customer.Email)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("failed to generate token"))
	}

	response := &authv1.SignUpResponse{
		Token: token,
	}

	return connect.NewResponse(response), nil
}

// SignIn implements the SignIn method of AuthService
func (s *Server) SignIn(ctx context.Context, req *connect.Request[authv1.SignInRequest]) (*connect.Response[authv1.SignInResponse], error) {
	email := req.Msg.GetEmail()
	password := req.Msg.GetPassword()

	// Validate input
	if email == "" || password == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("email and password are required"))
	}

	// Retrieve customer from database with password hash
	customer, err := s.db.read.GetCustomerByEmailWithPassword(ctx, email)
	if err != nil {
		// Customer not found
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("invalid credentials"))
	}

	// Verify the password
	err = bcrypt.CompareHashAndPassword([]byte(customer.PasswordHash), []byte(password))
	if err != nil {
		// Password doesn't match
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("invalid credentials"))
	}

	// Generate JWT token
	token, err := s.generateJWT(customer.Email)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("failed to generate token"))
	}

	response := &authv1.SignInResponse{
		Token: token,
	}

	return connect.NewResponse(response), nil
}

// generateJWT creates a new JWT token for the given email
func (s *Server) generateJWT(email string) (string, error) {
	claims := jwt.MapClaims{
		"email": email,
		"exp":   time.Now().Add(time.Hour * 24).Unix(), // Token expires in 24 hours
		"iat":   time.Now().Unix(),                     // Issued at
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString(s.jwtKey)
	if err != nil {
		return "", err
	}

	return tokenString, nil
}
