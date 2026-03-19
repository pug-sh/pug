package auth

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/fivebitsio/cotton/internal/core/orgs"
	"github.com/fivebitsio/cotton/internal/core/projects"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/fivebitsio/cotton/internal/slogx"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/xid"
	"golang.org/x/crypto/bcrypt"
)

var (
	ErrEmailAlreadyExists = errors.New("user with this email already exists")
	ErrInvalidCredentials = errors.New("invalid credentials")
)

const (
	aud = "cotton/dashboard"
	iss = "cotton/auth"
)

type Service struct {
	read   *dbread.Queries
	write  *dbwrite.Queries
	pgW    *pgxpool.Pool
	jwtKey []byte
}

func NewService(pgRO *pgxpool.Pool, pgW *pgxpool.Pool, jwtKey []byte) *Service {
	return &Service{
		read:   dbread.New(pgRO),
		write:  dbwrite.New(pgW),
		pgW:    pgW,
		jwtKey: jwtKey,
	}
}

func (s *Service) SignUpWithEmail(ctx context.Context, email, password string) (string, error) {
	_, err := s.read.GetCustomerByEmail(ctx, email)
	if err == nil {
		return "", ErrEmailAlreadyExists
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		slog.ErrorContext(ctx, "failed to check existing customer", slogx.Error(err))
		return "", err
	}

	passwordHash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		slog.ErrorContext(ctx, "failed to hash password", slogx.Error(err))
		return "", err
	}

	privKey, err := projects.NewPrivateKey()
	if err != nil {
		slog.ErrorContext(ctx, "failed to generate project private key", slogx.Error(err))
		return "", err
	}
	pubKey, err := projects.NewPublicKey()
	if err != nil {
		slog.ErrorContext(ctx, "failed to generate project public key", slogx.Error(err))
		return "", err
	}

	customerID := xid.New().String()
	orgID := xid.New().String()

	tx, err := s.pgW.Begin(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to begin transaction", slogx.Error(err))
		return "", err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	w := dbwrite.New(tx)

	if _, err = w.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID:           customerID,
		Email:        email,
		DisplayName:  "",
		PictureUri:   "",
		PasswordHash: string(passwordHash),
	}); err != nil {
		slog.ErrorContext(ctx, "failed to create customer", slogx.Error(err))
		return "", err
	}

	if _, err = w.CreateOrg(ctx, dbwrite.CreateOrgParams{
		ID:          orgID,
		DisplayName: "default",
	}); err != nil {
		slog.ErrorContext(ctx, "failed to create default org", slogx.Error(err))
		return "", err
	}

	if _, err = w.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID:      orgID,
		CustomerID: customerID,
		Role:       orgs.RoleAdmin,
	}); err != nil {
		slog.ErrorContext(ctx, "failed to add customer to default org", slogx.Error(err))
		return "", err
	}

	if _, err = w.CreateProject(ctx, dbwrite.CreateProjectParams{
		ID:            xid.New().String(),
		OrgID:         orgID,
		DisplayName:   "default",
		PrivateApiKey: privKey,
		PublicApiKey:  pubKey,
	}); err != nil {
		slog.ErrorContext(ctx, "failed to create default project", slogx.Error(err))
		return "", err
	}

	if err = tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "failed to commit signup transaction", slogx.Error(err))
		return "", err
	}

	token, err := s.generateJWT(customerID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to generate JWT", slogx.Error(err))
		return "", err
	}

	return token, nil
}

func (s *Service) SignInWithEmail(ctx context.Context, email, password string) (string, error) {
	customer, err := s.read.GetCustomerByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrInvalidCredentials
		}
		slog.ErrorContext(ctx, "failed to get customer by email", slogx.Error(err))
		return "", err
	}

	err = bcrypt.CompareHashAndPassword([]byte(customer.PasswordHash), []byte(password))
	if err != nil {
		if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
			return "", ErrInvalidCredentials
		}
		slog.ErrorContext(ctx, "failed to compare password hash", slogx.Error(err), slog.String("customerID", customer.ID))
		return "", err
	}

	token, err := s.generateJWT(customer.ID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to generate JWT", slogx.Error(err))
		return "", err
	}

	return token, nil
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
