package seed

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/fivebitsio/cotton/internal/core/projects"
	"github.com/jackc/pgx/v5"
	"github.com/rs/xid"
	"golang.org/x/crypto/bcrypt"
)

const (
	testEmail    = "test@cotton.dev"
	testPassword = "password"
	testName     = "Test User"
)

type Seeder struct {
	deps *deps
}

func NewSeeder(deps *deps) *Seeder {
	return &Seeder{deps: deps}
}

func (s *Seeder) Run(ctx context.Context) error {
	slog.InfoContext(ctx, "checking for existing test user")

	var existingID string
	err := s.deps.pg.QueryRow(ctx, "SELECT id FROM customers WHERE email = $1", testEmail).Scan(&existingID)
	if err == nil {
		slog.InfoContext(ctx, "test user already exists, skipping seed")
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("failed to check existing user: %w", err)
	}

	slog.InfoContext(ctx, "creating test user", slog.String("email", testEmail))

	passwordHash, err := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("failed to hash password: %w", err)
	}

	customerID := xid.New().String()
	_, err = s.deps.pg.Exec(ctx,
		`INSERT INTO customers (id, email, display_name, picture_uri, password_hash) VALUES ($1, $2, $3, $4, $5)`,
		customerID, testEmail, testName, "", string(passwordHash),
	)
	if err != nil {
		return fmt.Errorf("failed to create customer: %w", err)
	}

	slog.InfoContext(ctx, "creating default project", slog.String("customer_id", customerID))

	projectID := xid.New().String()
	privateKey := projects.NewPrivateKey()
	publicKey := projects.NewPublicKey()
	_, err = s.deps.pg.Exec(ctx,
		`INSERT INTO projects (id, customer_id, display_name, private_api_key, public_api_key) VALUES ($1, $2, $3, $4, $5)`,
		projectID, customerID, "default", privateKey, publicKey,
	)
	if err != nil {
		return fmt.Errorf("failed to create project: %w", err)
	}

	slog.InfoContext(ctx, "seed complete",
		slog.String("customer_id", customerID),
		slog.String("project_id", projectID),
		slog.String("private_api_key", privateKey),
		slog.String("public_api_key", publicKey),
	)

	return nil
}

func Run(ctx context.Context) error {
	d, err := newDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	return NewSeeder(d).Run(ctx)
}
