package seed_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/rs/xid"
	"golang.org/x/crypto/bcrypt"

	pgseed "github.com/pug-sh/pug/internal/app/seed/postgres"
	coreauth "github.com/pug-sh/pug/internal/core/auth"
	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	"github.com/pug-sh/pug/internal/core/projects"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
)

const adminEmail = "admin@pug.example"

// assertDemoOrgAdmin fails the test unless customerID is an admin of orgID and a
// member of no other org.
func assertDemoOrgAdmin(t *testing.T, ctx context.Context, read *dbread.Queries, customerID, orgID string) {
	t.Helper()
	orgs, err := read.GetOrgsWithRoleByCustomerID(ctx, customerID)
	if err != nil {
		t.Fatalf("GetOrgsWithRoleByCustomerID: %v", err)
	}
	if len(orgs) != 1 || orgs[0].ID != orgID || orgs[0].Role != coreorgs.RoleAdmin.String() {
		t.Fatalf("admin orgs = %+v, want admin of the demo org %s", orgs, orgID)
	}
}

// mustSeedAccount runs SeedAccount with PUG_DEMO_ADMIN_PASSWORD set to password,
// failing the test on an error.
func mustSeedAccount(t *testing.T, ctx context.Context, db *testutil.TestPostgres, password string) dbread.Project {
	t.Helper()
	t.Setenv("PUG_DEMO_ADMIN_PASSWORD", password)
	project, err := pgseed.SeedAccount(ctx, db.PgW)
	if err != nil {
		t.Fatalf("SeedAccount: %v", err)
	}
	return project
}

// mustCustomer returns the account at email, failing the test if there is none.
func mustCustomer(t *testing.T, ctx context.Context, read *dbread.Queries, email string) dbread.Customer {
	t.Helper()
	customer, err := read.GetCustomerByEmail(ctx, email)
	if err != nil {
		t.Fatalf("GetCustomerByEmail(%s): %v", email, err)
	}
	return customer
}

// TestSeedAccountDemoAdmin covers the demo accounts' passwords. The demo user is
// seeded without one, and a hash an old seed left behind is cleared. An admin
// exists only once PUG_DEMO_ADMIN_PASSWORD is set: created then, in the existing
// demo org, restored while the variable matches its password, and its password
// never overwritten.
func TestSeedAccountDemoAdmin(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	db := testutil.SetupPostgres(t)
	ctx := context.Background()
	read := dbread.New(db.PgW)
	write := dbwrite.New(db.PgW)

	project := mustSeedAccount(t, ctx, db, "")
	user := mustCustomer(t, ctx, read, coreauth.DemoUserEmail)
	if user.PasswordHash != "" {
		t.Error("the demo user was seeded with a password")
	}
	if _, err := read.GetCustomerByEmail(ctx, adminEmail); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("admin lookup with no password set: err = %v, want no rows", err)
	}

	// A database seeded while the demo user had a published password.
	oldHash, err := bcrypt.GenerateFromPassword([]byte("old-published-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("GenerateFromPassword: %v", err)
	}
	if _, err := write.UpdateCustomerPasswordHash(ctx, dbwrite.UpdateCustomerPasswordHashParams{
		ID: user.ID, PasswordHash: string(oldHash),
	}); err != nil {
		t.Fatalf("UpdateCustomerPasswordHash: %v", err)
	}
	mustSeedAccount(t, ctx, db, "")
	if mustCustomer(t, ctx, read, coreauth.DemoUserEmail).PasswordHash != "" {
		t.Error("re-seeding left the demo user's old password in place")
	}

	const first = "sixteen-chars-ok" // exactly 16 characters, the minimum: accepted
	mustSeedAccount(t, ctx, db, first)
	admin, err := read.GetCustomerByEmail(ctx, adminEmail)
	if err != nil {
		t.Fatalf("the admin was not created for the existing demo org: %v", err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(admin.PasswordHash), []byte(first)); err != nil {
		t.Errorf("admin password does not match PUG_DEMO_ADMIN_PASSWORD: %v", err)
	}
	assertDemoOrgAdmin(t, ctx, read, admin.ID, project.OrgID)

	// While the variable matches its password, a removed admin is put back and a
	// demoted one gets its role back.
	if _, err := write.DeleteOrgMember(ctx, dbwrite.DeleteOrgMemberParams{OrgID: project.OrgID, CustomerID: admin.ID}); err != nil {
		t.Fatalf("DeleteOrgMember: %v", err)
	}
	mustSeedAccount(t, ctx, db, first)
	assertDemoOrgAdmin(t, ctx, read, admin.ID, project.OrgID)
	demote := func() {
		t.Helper()
		if _, err := write.UpdateOrgMemberRole(ctx, dbwrite.UpdateOrgMemberRoleParams{
			Role: coreorgs.RoleViewer.String(), OrgID: project.OrgID, CustomerID: admin.ID,
		}); err != nil {
			t.Fatalf("UpdateOrgMemberRole: %v", err)
		}
	}
	demote()
	mustSeedAccount(t, ctx, db, first)
	assertDemoOrgAdmin(t, ctx, read, admin.ID, project.OrgID)

	// Unsetting the variable leaves the admin as it is.
	mustSeedAccount(t, ctx, db, "")
	assertDemoOrgAdmin(t, ctx, read, admin.ID, project.OrgID)

	// With a different password set, the account no longer proves it is the one
	// the seeder created: its role and its password are both left alone. The new
	// value is exactly 72 bytes, bcrypt's limit, so it passes the check.
	demote()
	mustSeedAccount(t, ctx, db, strings.Repeat("b", 72))
	if mustCustomer(t, ctx, read, adminEmail).PasswordHash != admin.PasswordHash {
		t.Error("re-seeding overwrote the admin's password")
	}
	role, err := read.GetOrgMemberRole(ctx, dbread.GetOrgMemberRoleParams{OrgID: project.OrgID, CustomerID: admin.ID})
	if err != nil {
		t.Fatalf("GetOrgMemberRole: %v", err)
	}
	if role != coreorgs.RoleViewer.String() {
		t.Errorf("with a mismatched password the admin's role became %q, want it left as viewer", role)
	}
}

// TestSeedAccountWontRestoreAnUnknownAdmin covers an account at the admin's
// address that the seeder didn't create, with no password, as a passwordless
// sign-in would make one. Setting the variable must not make it the demo org's
// admin.
func TestSeedAccountWontRestoreAnUnknownAdmin(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	db := testutil.SetupPostgres(t)
	ctx := context.Background()
	read := dbread.New(db.PgW)
	write := dbwrite.New(db.PgW)

	mustSeedAccount(t, ctx, db, "")
	stranger, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID: xid.New().String(), Email: adminEmail, DisplayName: "", PasswordHash: "", PictureUri: "",
	})
	if err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}

	mustSeedAccount(t, ctx, db, "a-long-admin-password")
	orgs, err := read.GetOrgsWithRoleByCustomerID(ctx, stranger.ID)
	if err != nil {
		t.Fatalf("GetOrgsWithRoleByCustomerID: %v", err)
	}
	if len(orgs) != 0 {
		t.Fatalf("an account the seeder didn't create joined %+v", orgs)
	}
	if mustCustomer(t, ctx, read, adminEmail).PasswordHash != "" {
		t.Error("the seeder set a password on an account it didn't create")
	}
}

// TestSeedAccountFindsTheSharedDemoOrg pins how the seeder finds the demo org on
// an existing database: the demo user's org that also has the demo viewer. An
// org created as the demo user while its password was public must never be
// taken for it.
func TestSeedAccountFindsTheSharedDemoOrg(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	db := testutil.SetupPostgres(t)
	ctx := context.Background()
	read := dbread.New(db.PgW)
	write := dbwrite.New(db.PgW)

	project := mustSeedAccount(t, ctx, db, "")
	user := mustCustomer(t, ctx, read, coreauth.DemoUserEmail)

	// An org created as the demo user, with a project of its own.
	planted, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: xid.New().String(), DisplayName: "planted"})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	// Backdated so it is the demo user's first org: without the viewer check, the
	// seeder would take it.
	if _, err := db.PgW.Exec(ctx, "update orgs set create_time = create_time - interval '1 year' where id = $1", planted.ID); err != nil {
		t.Fatalf("backdate the planted org: %v", err)
	}
	if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID: planted.ID, CustomerID: user.ID, Role: coreorgs.RoleAdmin.String(),
	}); err != nil {
		t.Fatalf("CreateOrgMember: %v", err)
	}
	if _, err := projects.CreateProjectInTx(ctx, write, planted.ID, "planted", ""); err != nil {
		t.Fatalf("CreateProjectInTx: %v", err)
	}
	if again := mustSeedAccount(t, ctx, db, ""); again.ID != project.ID {
		t.Fatalf("resolved project %s, want the demo project %s", again.ID, project.ID)
	}

	// Removed from the demo org, the demo user's only org is the planted one, which
	// has no demo viewer: the seeder refuses rather than seeding into it.
	if _, err := write.DeleteOrgMember(ctx, dbwrite.DeleteOrgMemberParams{OrgID: project.OrgID, CustomerID: user.ID}); err != nil {
		t.Fatalf("DeleteOrgMember: %v", err)
	}
	if _, err := pgseed.SeedAccount(ctx, db.PgW); err == nil || !strings.Contains(err.Error(), "no org has both") {
		t.Fatalf("SeedAccount without the demo user in the demo org: err = %v, want a no-shared-org error", err)
	}
}

// TestSeedAccountRebuildsADeletedDemoProject covers the demo rebuild: once the
// operator deletes the demo project, the next seed gives the demo org a new one,
// which can seed its profiles before the purge runs, and later seeds keep it.
func TestSeedAccountRebuildsADeletedDemoProject(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	db := testutil.SetupPostgres(t)
	ctx := context.Background()
	users := []int{0, 1, 2}

	old := mustSeedAccount(t, ctx, db, "")
	if err := pgseed.SeedProfilesForUsers(ctx, db.PgW, old.ID, users); err != nil {
		t.Fatalf("SeedProfilesForUsers(old): %v", err)
	}
	if _, err := projects.NewService(db.PgW, db.PgW, nil).DeleteProjectByOperator(ctx, old.ID, "test"); err != nil {
		t.Fatalf("DeleteProjectByOperator: %v", err)
	}

	rebuilt := mustSeedAccount(t, ctx, db, "")
	if rebuilt.ID == old.ID || rebuilt.OrgID != old.OrgID {
		t.Fatalf("rebuilt project %+v, want a new project in the demo org %s", rebuilt, old.OrgID)
	}
	live, err := dbread.New(db.PgW).GetProjectsByOrgID(ctx, old.OrgID)
	if err != nil {
		t.Fatalf("GetProjectsByOrgID: %v", err)
	}
	if len(live) != 1 || live[0].ID != rebuilt.ID {
		t.Fatalf("the demo org's live projects = %+v, want only %s", live, rebuilt.ID)
	}
	if err := pgseed.SeedProfilesForUsers(ctx, db.PgW, rebuilt.ID, users); err != nil {
		t.Fatalf("SeedProfilesForUsers(rebuilt): %v", err)
	}
	if n, err := pgseed.CountDemoProfiles(ctx, db.PgW, rebuilt.ID); err != nil || n != int64(len(users)) {
		t.Fatalf("rebuilt project's demo profiles = %d (err %v), want %d", n, err, len(users))
	}
	if again := mustSeedAccount(t, ctx, db, ""); again.ID != rebuilt.ID {
		t.Fatalf("a later seed resolved %s, want the rebuilt project %s", again.ID, rebuilt.ID)
	}
}

// TestSeedAccountWithoutTheDemoViewer covers a seed from before the demo viewer
// existed. With no viewer account nothing marks the demo org, so the seeder
// refuses rather than take the demo user's first org.
func TestSeedAccountWithoutTheDemoViewer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	db := testutil.SetupPostgres(t)
	ctx := context.Background()

	mustSeedAccount(t, ctx, db, "")
	viewer := mustCustomer(t, ctx, dbread.New(db.PgW), coreauth.DemoViewerEmail)
	for _, stmt := range []string{
		"delete from org_members where customer_id = $1",
		"delete from customers where id = $1",
	} {
		if _, err := db.PgW.Exec(ctx, stmt, viewer.ID); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	if _, err := pgseed.SeedAccount(ctx, db.PgW); err == nil || !strings.Contains(err.Error(), "to find the demo org by") {
		t.Fatalf("SeedAccount without a demo viewer: err = %v, want a missing-viewer error", err)
	}
}

// TestSeedAccountRefusesAnUnreadableAdminHash covers an account at the admin's
// address whose stored hash isn't a bcrypt hash. The seeder can't tell whether
// the variable matches, so it fails rather than warn about a mismatch that no
// value of the variable could fix.
func TestSeedAccountRefusesAnUnreadableAdminHash(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	db := testutil.SetupPostgres(t)
	ctx := context.Background()
	write := dbwrite.New(db.PgW)

	mustSeedAccount(t, ctx, db, "")
	if _, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID: xid.New().String(), Email: adminEmail, DisplayName: "", PasswordHash: "not-a-bcrypt-hash", PictureUri: "",
	}); err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}

	t.Setenv("PUG_DEMO_ADMIN_PASSWORD", "a-long-admin-password")
	if _, err := pgseed.SeedAccount(ctx, db.PgW); err == nil || !strings.Contains(err.Error(), "check the demo admin's password") {
		t.Fatalf("SeedAccount with an unreadable admin hash: err = %v, want a password-check error", err)
	}
}

// TestSeedAccountDemoAdminOnFirstSeed covers a new deployment that sets the
// variable from the start: the admin exists after the very first seed.
func TestSeedAccountDemoAdminOnFirstSeed(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	db := testutil.SetupPostgres(t)
	ctx := context.Background()
	read := dbread.New(db.PgW)

	project := mustSeedAccount(t, ctx, db, "first-boot-password")
	admin, err := read.GetCustomerByEmail(ctx, adminEmail)
	if err != nil {
		t.Fatalf("the first seed created no admin: %v", err)
	}
	assertDemoOrgAdmin(t, ctx, read, admin.ID, project.OrgID)
}

// TestSeedAccountRejectsBadAdminPassword pins the bounds on
// PUG_DEMO_ADMIN_PASSWORD: 16 characters to 72 bytes, with no surrounding
// whitespace. The accepted edges are covered in TestSeedAccountDemoAdmin. The
// check runs before the database is touched.
func TestSeedAccountRejectsBadAdminPassword(t *testing.T) {
	for name, password := range map[string]string{
		"under 16 characters":       "fifteen-chars!!",
		"a trailing newline":        "a-long-secret-password\n",
		"a leading space":           " a-long-secret-password",
		"8 characters in 16 bytes":  strings.Repeat(string(rune(0xE9)), 8),
		"over 72 bytes":             strings.Repeat("a", 73),
		"37 characters in 74 bytes": strings.Repeat(string(rune(0xE9)), 37),
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("PUG_DEMO_ADMIN_PASSWORD", password)
			_, err := pgseed.SeedAccount(context.Background(), nil)
			if err == nil || !strings.Contains(err.Error(), "PUG_DEMO_ADMIN_PASSWORD") {
				t.Fatalf("err = %v, want a PUG_DEMO_ADMIN_PASSWORD error", err)
			}
			if strings.Contains(err.Error(), password) {
				t.Fatal("the error includes the password")
			}
		})
	}
}

// TestSeedAccountApiKeys covers what the demo deployment and `pug seed` depend
// on: the seeded project comes with a working public key, and with the private
// key the seeder creates explicitly for local SDK/MCP calls. Re-running resolves
// the same project rather than seeding a second one.
func TestSeedAccountApiKeys(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	db := testutil.SetupPostgres(t)
	ctx := context.Background()

	project := mustSeedAccount(t, ctx, db, "")

	svc := projects.NewService(db.PgRO, db.PgW, nil)
	keys, err := svc.ListApiKeys(ctx, project.ID)
	if err != nil {
		t.Fatalf("ListApiKeys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("got %d keys, want 2 (the starter public key and the seeded private one)", len(keys))
	}

	byKind := map[string]dbread.ApiKey{}
	for _, k := range keys {
		byKind[k.Kind] = k
	}

	pub, ok := byKind[string(projects.KindPublic)]
	if !ok {
		t.Fatal("the seeded project has no public key")
	}
	// A public key is stored whole, so the token IS the key the SDK snippet pastes.
	if !strings.HasPrefix(pub.Token, "pub_") {
		t.Errorf("public key token = %q, want a pub_ key", pub.Token)
	}

	// The private key is only reachable as a digest here; that it exists at all is
	// what `pug worker demo` and local SDK calls need.
	if _, ok := byKind[string(projects.KindPrivate)]; !ok {
		t.Error("the seeded project has no private key, so local dev has nothing to call the SDK/MCP endpoints with")
	}

	t.Run("re-seeding resolves the same project", func(t *testing.T) {
		if again := mustSeedAccount(t, ctx, db, ""); again.ID != project.ID {
			t.Errorf("second run returned project %q, want the first run's %q", again.ID, project.ID)
		}

		keys, err := svc.ListApiKeys(ctx, project.ID)
		if err != nil {
			t.Fatalf("ListApiKeys: %v", err)
		}
		if len(keys) != 2 {
			t.Errorf("got %d keys after re-seeding, want the original 2", len(keys))
		}
	})
}
