package auth_test

import (
	"context"
	"errors"
	"testing"

	"github.com/golang-jwt/jwt/v5"

	"github.com/pug-sh/pug/internal/core/auth"
	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	"github.com/pug-sh/pug/internal/core/projects"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
)

// TestDemoSignIn covers the credential-less demo viewer login: it is gated by
// the demo flag, requires the demo account to be seeded, and — once both hold —
// mints a session for the seeded viewer scoped to the demo project.
func TestDemoSignIn(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	db := testutil.SetupPostgres(t)
	ctx := context.Background()
	jwtKey := []byte("test-secret-key-for-jwt")

	// svcOff keeps the demo login disabled (the default); svcOn enables it. Both
	// share the same pools, so seeding below is visible to svcOn.
	svcOff := mustNewTestAuthService(t, db, &stubPublisher{})
	svcOn := mustNewTestAuthService(t, db, &stubPublisher{})
	svcOn.SetDemoEnabledForTest(true)

	t.Run("disabled returns ErrDemoUnavailable", func(t *testing.T) {
		if _, err := svcOff.DemoSignIn(ctx); !errors.Is(err, auth.ErrDemoUnavailable) {
			t.Fatalf("err = %v, want ErrDemoUnavailable", err)
		}
	})

	t.Run("enabled but unseeded returns ErrDemoUnavailable", func(t *testing.T) {
		if _, err := svcOn.DemoSignIn(ctx); !errors.Is(err, auth.ErrDemoUnavailable) {
			t.Fatalf("err = %v, want ErrDemoUnavailable", err)
		}
	})

	// Seed the demo viewer exactly as the demo seeder does: customer (snoop) +
	// org + ORG_ROLE_VIEWER membership + project.
	write := dbwrite.New(db.PgW)
	viewer, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID: "cust-demo-viewer", Email: auth.DemoViewerEmail, DisplayName: "Snoop Pugg",
	})
	if err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}
	org, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: "org-demo", DisplayName: "default"})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID: org.ID, CustomerID: viewer.ID, Role: coreorgs.RoleViewer.String(),
	}); err != nil {
		t.Fatalf("CreateOrgMember: %v", err)
	}
	proj, err := projects.CreateProjectInTx(ctx, write, org.ID, "default", "")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	t.Run("enabled and seeded mints a viewer session scoped to the demo project", func(t *testing.T) {
		demo, err := svcOn.DemoSignIn(ctx)
		if err != nil {
			t.Fatalf("DemoSignIn: %v", err)
		}
		if demo.Session.AccessToken == "" {
			t.Error("expected non-empty access token")
		}
		if demo.Session.RefreshToken == "" {
			t.Error("expected non-empty refresh token")
		}
		if demo.ProjectID != proj.ID {
			t.Errorf("project id = %q, want %q", demo.ProjectID, proj.ID)
		}

		// The minted session must be for the demo VIEWER specifically — the JWT
		// subject is what WithJWTAuth resolves to a customer, so this pins that the
		// principal is snoop (and thus a read-only org member), not anyone else.
		var claims jwt.RegisteredClaims
		if _, err := jwt.ParseWithClaims(demo.Session.AccessToken, &claims, func(*jwt.Token) (any, error) {
			return jwtKey, nil
		}); err != nil {
			t.Fatalf("parse access token: %v", err)
		}
		if claims.Subject != viewer.ID {
			t.Errorf("subject = %q, want demo viewer id %q", claims.Subject, viewer.ID)
		}
	})
}

// TestDemoSignInRejectsNonViewer pins the defense-in-depth role check: even with
// the demo flag on and the account fully seeded (org + membership + project),
// DemoSignIn refuses to mint unless the resolved demo account is genuinely an
// ORG_ROLE_VIEWER. This is the runtime guard on the read-only invariant of the
// public, credential-less endpoint — a mis-seed or a later promotion of snoop to
// a write-capable role must fail closed rather than yield an elevated session.
func TestDemoSignInRejectsNonViewer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	db := testutil.SetupPostgres(t)
	ctx := context.Background()

	svcOn := mustNewTestAuthService(t, db, &stubPublisher{})
	svcOn.SetDemoEnabledForTest(true)

	// Seed snoop exactly as the demo seeder does EXCEPT as a MEMBER, not a viewer.
	write := dbwrite.New(db.PgW)
	snoop, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID: "cust-demo-snoop", Email: auth.DemoViewerEmail, DisplayName: "Snoop Pugg",
	})
	if err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}
	org, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: "org-demo", DisplayName: "default"})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID: org.ID, CustomerID: snoop.ID, Role: coreorgs.RoleMember.String(),
	}); err != nil {
		t.Fatalf("CreateOrgMember: %v", err)
	}
	if _, err := projects.CreateProjectInTx(ctx, write, org.ID, "default", ""); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	if _, err := svcOn.DemoSignIn(ctx); !errors.Is(err, auth.ErrDemoUnavailable) {
		t.Fatalf("err = %v, want ErrDemoUnavailable (a non-viewer account must not mint a session)", err)
	}
}

// TestDemoSignInUnavailableWhenHalfSeeded pins the two half-seeded failure modes
// resolveDemoProjectID guards: the demo viewer customer exists (so the email
// lookup succeeds), but the rest of the demo graph is incomplete. A viewer with
// no org membership, or a viewer whose org has no project, is a mis-/half-seed —
// and the credential-less endpoint must fail closed with ErrDemoUnavailable
// rather than mint a session it cannot scope to a demo project.
func TestDemoSignInUnavailableWhenHalfSeeded(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := context.Background()

	t.Run("viewer customer exists but has no org", func(t *testing.T) {
		db := testutil.SetupPostgres(t)
		svcOn := mustNewTestAuthService(t, db, &stubPublisher{})
		svcOn.SetDemoEnabledForTest(true)

		write := dbwrite.New(db.PgW)
		if _, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
			ID: "cust-demo-viewer", Email: auth.DemoViewerEmail, DisplayName: "Snoop Pugg",
		}); err != nil {
			t.Fatalf("CreateCustomer: %v", err)
		}

		if _, err := svcOn.DemoSignIn(ctx); !errors.Is(err, auth.ErrDemoUnavailable) {
			t.Fatalf("err = %v, want ErrDemoUnavailable (a viewer with no org must not mint)", err)
		}
	})

	t.Run("viewer in an org but the org has no project", func(t *testing.T) {
		db := testutil.SetupPostgres(t)
		svcOn := mustNewTestAuthService(t, db, &stubPublisher{})
		svcOn.SetDemoEnabledForTest(true)

		write := dbwrite.New(db.PgW)
		viewer, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
			ID: "cust-demo-viewer", Email: auth.DemoViewerEmail, DisplayName: "Snoop Pugg",
		})
		if err != nil {
			t.Fatalf("CreateCustomer: %v", err)
		}
		org, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: "org-demo", DisplayName: "default"})
		if err != nil {
			t.Fatalf("CreateOrg: %v", err)
		}
		if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
			OrgID: org.ID, CustomerID: viewer.ID, Role: coreorgs.RoleViewer.String(),
		}); err != nil {
			t.Fatalf("CreateOrgMember: %v", err)
		}
		// Deliberately create no project for the org.

		if _, err := svcOn.DemoSignIn(ctx); !errors.Is(err, auth.ErrDemoUnavailable) {
			t.Fatalf("err = %v, want ErrDemoUnavailable (a viewer whose org has no project must not mint)", err)
		}
	})
}

// TestDemoSignInPropagatesLookupError pins that a transient infrastructure error
// during the demo account lookup (forced here with a cancelled context) surfaces
// as the raw error — which the handler maps to CodeInternal — and is NOT collapsed
// into ErrDemoUnavailable. ErrDemoUnavailable is reserved for the demo being off
// or genuinely unseeded (pgx.ErrNoRows); a DB outage is a distinct, retryable
// state, so DemoSignIn must not make a failing database look like an unseeded one.
func TestDemoSignInPropagatesLookupError(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	db := testutil.SetupPostgres(t)
	svcOn := mustNewTestAuthService(t, db, &stubPublisher{})
	svcOn.SetDemoEnabledForTest(true)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := svcOn.DemoSignIn(cancelled)
	if err == nil {
		t.Fatal("expected an error from a cancelled context, got nil")
	}
	if errors.Is(err, auth.ErrDemoUnavailable) {
		t.Fatalf("err = %v, want a non-ErrDemoUnavailable infrastructure error (a DB failure must not look like 'unseeded')", err)
	}
}
