package auth

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"
	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/slogx"
)

// DemoViewerEmail is the email of the seeded read-only demo account ("Snoop
// Pugg"). DemoSignIn mints a viewer session for this customer, and the demo
// seeder (internal/app/seed/postgres) provisions it as an ORG_ROLE_VIEWER member
// of the demo org — this const is the single source of truth shared by both, so
// the login target and the seeded account can never drift apart.
const DemoViewerEmail = "snoop@pug.sh"

// ErrDemoUnavailable is returned by DemoSignIn when the demo login is disabled
// (PUG_DEMO_ENABLED off) or the demo viewer account/project has not been seeded.
// The handler maps it to a CodeUnavailable response.
var ErrDemoUnavailable = errors.New("demo sign-in unavailable")

// DemoSession is the result of DemoSignIn: a viewer Session plus the demo
// project the caller must scope to via the x-project-id header to see the
// seeded demo data.
type DemoSession struct {
	Session   Session
	ProjectID string
}

// DemoSignIn mints a full session for the read-only demo viewer account without
// credentials, so a visitor landing on the public demo page is authenticated in
// viewer mode. The session's identity is the seeded snoop@pug.sh customer — a
// ORG_ROLE_VIEWER member of the demo org — so the existing RBAC makes the
// resulting principal genuinely read-only; there is no demo-specific
// authorization path. As defense-in-depth on this credential-less endpoint,
// resolveDemoProjectID additionally refuses to mint unless the account really is
// a viewer, so a mis-seed or a later promotion of snoop fails closed rather than
// handing an anonymous caller a write-capable session. Returns ErrDemoUnavailable
// when the demo login is disabled, the demo account/project has not been seeded,
// or the account is not a viewer.
func (s *Service) DemoSignIn(ctx context.Context) (DemoSession, error) {
	if !s.demoEnabled {
		return DemoSession{}, ErrDemoUnavailable
	}

	customer, err := s.read.GetCustomerByEmail(ctx, DemoViewerEmail)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Login is enabled but the account is not seeded — unavailable, not
			// internal; the operator just needs to run the demo seed/worker.
			slog.WarnContext(ctx, "demo sign-in enabled but demo viewer account not seeded", slog.String("email", DemoViewerEmail))
			return DemoSession{}, ErrDemoUnavailable
		}
		slog.ErrorContext(ctx, "demo sign-in: failed to look up demo viewer", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return DemoSession{}, err
	}

	projectID, err := s.resolveDemoProjectID(ctx, customer.ID)
	if err != nil {
		return DemoSession{}, err
	}

	session, err := s.issueSession(ctx, customer.ID)
	if err != nil {
		// issueSession logs + records at source.
		return DemoSession{}, err
	}

	return DemoSession{Session: session, ProjectID: projectID}, nil
}

// resolveDemoProjectID resolves the demo project the viewer should scope to and
// verifies the account is genuinely read-only. It takes the viewer's oldest org
// (GetOrgsWithRoleByCustomerID orders by create_time) — the demo seeder makes
// snoop a member of exactly that one org — checks its membership role is
// ORG_ROLE_VIEWER, then returns that org's oldest project (GetProjectsByOrgID is
// likewise create_time-ordered, so it deterministically matches the seeder's
// resolveProject). A missing org/project, or a non-viewer role, means the demo
// is mis- or half-seeded → unavailable.
func (s *Service) resolveDemoProjectID(ctx context.Context, customerID string) (string, error) {
	orgs, err := s.read.GetOrgsWithRoleByCustomerID(ctx, customerID)
	if err != nil {
		slog.ErrorContext(ctx, "demo sign-in: failed to resolve demo org", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return "", err
	}
	if len(orgs) == 0 {
		slog.WarnContext(ctx, "demo sign-in: demo viewer has no org", slog.String("customer_id", customerID))
		return "", ErrDemoUnavailable
	}

	// Enforce the read-only invariant at the mint site. The seed makes snoop an
	// ORG_ROLE_VIEWER, but this is a public, credential-less endpoint, so verify
	// the role rather than trust the seed: a mis-seed or a later promotion must
	// fail closed, never silently hand an anonymous caller a write-capable session.
	role, err := coreorgs.ParseRole(orgs[0].Role)
	if err != nil {
		// An unrecognized stored role is corrupted data, not an expected business
		// state — record it at the detecting layer so it's tracked distinctly from
		// a valid-but-non-viewer account. Still fail closed.
		slog.ErrorContext(ctx, "demo sign-in: demo account has an unrecognized stored role; refusing to mint",
			slogx.Error(err), slog.String("org_id", orgs[0].ID), slog.String("role", orgs[0].Role))
		telemetry.RecordError(ctx, err)
		return "", ErrDemoUnavailable
	}
	if role != coreorgs.RoleViewer {
		slog.WarnContext(ctx, "demo sign-in: demo account is not a viewer; refusing to mint",
			slog.String("org_id", orgs[0].ID), slog.String("role", orgs[0].Role))
		return "", ErrDemoUnavailable
	}

	projects, err := s.read.GetProjectsByOrgID(ctx, orgs[0].ID)
	if err != nil {
		slog.ErrorContext(ctx, "demo sign-in: failed to resolve demo project", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return "", err
	}
	if len(projects) == 0 {
		slog.WarnContext(ctx, "demo sign-in: demo org has no project", slog.String("org_id", orgs[0].ID))
		return "", ErrDemoUnavailable
	}

	return projects[0].ID, nil
}
