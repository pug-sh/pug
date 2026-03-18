package seed

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/fivebitsio/cotton/internal/core/orgs"
	"github.com/fivebitsio/cotton/internal/core/projects"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
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
	read := dbread.New(s.deps.pg)
	write := dbwrite.New(s.deps.pg)
	orgsSvc := orgs.NewService(s.deps.pg, s.deps.pg)
	projectsSvc := projects.NewService(s.deps.pg, s.deps.pg, nil)

	slog.InfoContext(ctx, "checking for existing test user")

	_, err := read.GetCustomerByEmail(ctx, testEmail)
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

	customer, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID:           xid.New().String(),
		Email:        testEmail,
		DisplayName:  testName,
		PasswordHash: string(passwordHash),
		PictureUri:   "",
	})
	if err != nil {
		return fmt.Errorf("failed to create customer: %w", err)
	}

	org, err := orgsSvc.CreateOrg(ctx, "default")
	if err != nil {
		return fmt.Errorf("failed to create default org: %w", err)
	}

	if _, err = orgsSvc.AddMember(ctx, org.ID, customer.ID, orgs.RoleAdmin); err != nil {
		return fmt.Errorf("failed to add customer to org: %w", err)
	}

	slog.InfoContext(ctx, "creating default project", slog.String("customer_id", customer.ID))

	project, err := projectsSvc.CreateProject(ctx, org.ID, customer.ID, "default")
	if err != nil {
		return fmt.Errorf("failed to create project: %w", err)
	}

	slog.InfoContext(ctx, "seed complete",
		slog.String("customer_id", customer.ID),
		slog.String("org_id", org.ID),
		slog.String("project_id", project.ID),
		slog.String("public_api_key", project.PublicApiKey),
		slog.String("private_api_key", project.PrivateApiKey),
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
