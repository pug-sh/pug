package seed

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	chseed "github.com/pug-sh/pug/internal/app/seed/clickhouse"
	coreauth "github.com/pug-sh/pug/internal/core/auth"
	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	"github.com/pug-sh/pug/internal/core/projects"
	dbtypes "github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/rs/xid"
	"golang.org/x/crypto/bcrypt"
)

const (
	// The demo user owns the demo org and its project. It has no password, and the
	// server keeps it read-only (coreauth.IsDemoAccount): its password was once
	// published, and a database seeded back then holds it until seedAccount runs.
	userEmail = coreauth.DemoUserEmail
	userName  = "Pug"

	// A second demo account with read-only (viewer) access to the same org so the
	// viewer role can be exercised in the dashboard. Snoop Pugg snoops: looks but
	// never touches. It has no password: DemoSignIn is its only way in. The email
	// is sourced from coreauth so the seeded account and AuthService.DemoSignIn's
	// login target are one constant and cannot drift apart.
	viewerEmail = coreauth.DemoViewerEmail
	viewerName  = "Snoop Pugg"

	// The demo org's admin, created only when PUG_DEMO_ADMIN_PASSWORD is set, so a
	// usable admin exists only where an operator chose its password. The address
	// can't receive mail (RFC 2606's .example).
	adminEmail = "admin@pug.example"
	adminName  = "Pug Admin"
)

type Seeder struct {
	deps *deps
}

func NewSeeder(deps *deps) *Seeder {
	return &Seeder{deps: deps}
}

// seedAccount ensures the demo customer/org/project exists and returns the
// project, without seeding any profiles. If the demo customer already exists its
// project is resolved and reused, so this is safe to call on every worker start.
// Profile seeding is a separate, event-gated step (SeedProfilesForUsers) so a
// profile is only ever created for a user that has events.
func (s *Seeder) seedAccount(ctx context.Context) (dbread.Project, error) {
	project, err := s.demoProject(ctx)
	if err != nil {
		return dbread.Project{}, err
	}
	// Runs whether the demo user is new or found, so setting the password later
	// still gives an existing demo org its admin.
	if err := s.ensureAdmin(ctx, project.OrgID); err != nil {
		return dbread.Project{}, err
	}
	return project, nil
}

// demoProject resolves the demo user's project, or creates the demo user, org
// and project on a fresh database.
func (s *Seeder) demoProject(ctx context.Context) (dbread.Project, error) {
	read := dbread.New(s.deps.pg)

	slog.InfoContext(ctx, "checking for existing demo user")

	customer, err := read.GetCustomerByEmail(ctx, userEmail)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return dbread.Project{}, fmt.Errorf("failed to check existing user: %w", err)
	}
	if err == nil {
		// A seed from before the demo user lost its password left the published
		// hash behind. Sign-in refuses the account anyway; this removes the hash.
		if customer.PasswordHash != "" {
			if _, err := dbwrite.New(s.deps.pg).UpdateCustomerPasswordHash(ctx, dbwrite.UpdateCustomerPasswordHashParams{
				ID: customer.ID, PasswordHash: "",
			}); err != nil {
				return dbread.Project{}, fmt.Errorf("failed to clear the demo user's old password: %w", err)
			}
			slog.InfoContext(ctx, "cleared the demo user's old password")
		}
		slog.InfoContext(ctx, "demo user already exists, resolving project")
		return s.resolveProject(ctx, read, customer.ID)
	}

	slog.InfoContext(ctx, "creating demo user", slog.String("email", userEmail))
	project, privateKey, err := s.seedCustomerOrgProject(ctx)
	if err != nil {
		return dbread.Project{}, err
	}
	// Only the private key's digest is stored, so this log is the one and only
	// place it appears. If it is missed, mint another from the project settings
	// page rather than reaching for the database. The project's public key is not
	// logged: unlike this one it stays readable, via ListApiKeys.
	slog.DebugContext(ctx, "account seed complete",
		slog.String("project_id", project.ID),
		slog.String("private_api_key", privateKey),
	)
	return project, nil
}

// ensureAdmin gives the demo org an admin when PUG_DEMO_ADMIN_PASSWORD is set,
// since both demo accounts are read-only. It creates the account if missing. An
// existing account gets its admin role back only while its password matches the
// variable, since an account at that address need not be the one the seeder
// created. It never changes the account's password or logs it.
func (s *Seeder) ensureAdmin(ctx context.Context, orgID string) error {
	if s.deps.adminPassword == "" {
		slog.InfoContext(ctx, "PUG_DEMO_ADMIN_PASSWORD is not set, so the seeder creates or restores no demo admin")
		return nil
	}
	existing, err := dbread.New(s.deps.pg).GetCustomerByEmail(ctx, adminEmail)
	if errors.Is(err, pgx.ErrNoRows) {
		return s.createAdmin(ctx, orgID)
	}
	if err != nil {
		return fmt.Errorf("failed to check existing admin: %w", err)
	}
	// The seeder always sets a password, and nothing clears one, so an account
	// without one came from a passwordless sign-in. No value of the variable can
	// match it.
	if existing.PasswordHash == "" {
		slog.WarnContext(ctx, "an account with no password holds the demo admin's address, so the seeder "+
			"didn't create it and won't make it the admin; check where it came from",
			slog.String("email", adminEmail), slog.String("customer_id", existing.ID))
		return nil
	}
	err = bcrypt.CompareHashAndPassword([]byte(existing.PasswordHash), []byte(s.deps.adminPassword))
	if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
		slog.WarnContext(ctx, "the account at the demo admin's address doesn't have PUG_DEMO_ADMIN_PASSWORD as its "+
			"password, so the seeder leaves its role alone; if you changed it in the dashboard, unset the "+
			"variable or set it to match, which needs a password of 16 characters or more",
			slog.String("email", adminEmail))
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to check the demo admin's password: %w", err)
	}
	return s.restoreAdmin(ctx, orgID, existing.ID)
}

// createAdmin creates the admin account with PUG_DEMO_ADMIN_PASSWORD as its
// password, as the demo org's admin.
func (s *Seeder) createAdmin(ctx context.Context, orgID string) error {
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(s.deps.adminPassword), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("failed to hash the admin password: %w", err)
	}

	tx, err := s.deps.pg.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	w := dbwrite.New(tx)
	admin, err := w.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID:           xid.New().String(),
		Email:        adminEmail,
		DisplayName:  adminName,
		PasswordHash: string(passwordHash),
		PictureUri:   "",
	})
	if err != nil {
		return fmt.Errorf("failed to create admin customer: %w", err)
	}
	if _, err := w.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID:      orgID,
		CustomerID: admin.ID,
		Role:       coreorgs.RoleAdmin.String(),
	}); err != nil {
		return fmt.Errorf("failed to add admin to org: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("failed to commit admin seed: %w", err)
	}
	slog.InfoContext(ctx, "created demo admin", slog.String("email", adminEmail))
	return nil
}

// restoreAdmin makes the existing admin account the demo org's admin again. The
// demo user counts as an admin, so the last-admin guard lets this one leave, be
// removed or be demoted, and each is undone here. ensureAdmin calls it only
// while the account's password matches PUG_DEMO_ADMIN_PASSWORD.
func (s *Seeder) restoreAdmin(ctx context.Context, orgID, customerID string) error {
	w := dbwrite.New(s.deps.pg)
	added, err := w.CreateOrgMemberIfAbsent(ctx, dbwrite.CreateOrgMemberIfAbsentParams{
		CustomerID: customerID, OrgID: orgID, Role: coreorgs.RoleAdmin.String(),
	})
	if err != nil {
		return fmt.Errorf("failed to re-add the admin to the demo org: %w", err)
	}
	if added > 0 {
		slog.WarnContext(ctx, "the demo admin had left or been removed from the demo org, so the seeder added it back",
			slog.String("email", adminEmail))
		return nil
	}
	role, err := dbread.New(s.deps.pg).GetOrgMemberRole(ctx, dbread.GetOrgMemberRoleParams{OrgID: orgID, CustomerID: customerID})
	if err != nil {
		return fmt.Errorf("failed to read the admin's role: %w", err)
	}
	if role == coreorgs.RoleAdmin.String() {
		return nil
	}
	if _, err := w.UpdateOrgMemberRole(ctx, dbwrite.UpdateOrgMemberRoleParams{
		Role: coreorgs.RoleAdmin.String(), OrgID: orgID, CustomerID: customerID,
	}); err != nil {
		return fmt.Errorf("failed to restore the admin's role: %w", err)
	}
	slog.WarnContext(ctx, "the demo admin had been demoted, so the seeder made it the admin again",
		slog.String("email", adminEmail), slog.String("was", role))
	return nil
}

// seedProfilesForUsers seeds Postgres profiles, devices and merges for exactly
// the given user indices (parsed from the backfill's emitted distinct ids), so a
// profile exists only for a user with events.
func (s *Seeder) seedProfilesForUsers(ctx context.Context, projectID string, indices []int) error {
	// Idempotency: skip if this project already has seeded profiles. seedProfiles
	// and seedDevices upsert, but seedMerges mints fresh xid-keyed anonymous rows
	// every run, so re-seeding a populated project would accumulate junk.
	//
	// The skip is a coarse "any profiles?" gate, not proof a prior run finished
	// all three steps (profiles + devices + merges). That is safe because the
	// callers that could reach a populated project never want a re-seed here:
	//   - The worker only calls this on an empty project (gated on zero events,
	//     and a profile never exists without events). A crash after events commit
	//     but before/during the ClickHouse copy is reconciled one level up by
	//     ensureSeed (healDemoProfiles re-drives the idempotent copy from the
	//     Postgres set) rather than looping back through here, so the worker
	//     never re-seeds Postgres against a stale partial set.
	//   - The CLI `seed --no-reset` path clears the demo profiles first
	//     (ResetDemoProfiles), so it falls through to a real re-seed.
	// A finer completion marker wouldn't help: synthetic events/merges carry
	// random ids the ReplacingMergeTree can't dedup across runs, so re-running
	// repairs nothing; a manual reset (`pug seed`) is the documented recovery.
	var existing int64
	if err := s.deps.pg.QueryRow(ctx,
		"SELECT count(*) FROM profiles WHERE project_id = $1 AND id LIKE 'user-%'", projectID,
	).Scan(&existing); err != nil {
		return fmt.Errorf("count demo profiles: %w", err)
	}
	if existing > 0 {
		slog.InfoContext(ctx, "demo profiles already seeded, skipping",
			slog.String("project_id", projectID),
			slog.Int64("profiles", existing),
		)
		return nil
	}

	identifiedIDs, err := s.seedProfiles(ctx, projectID, indices)
	if err != nil {
		return fmt.Errorf("failed to seed profiles: %w", err)
	}
	if err := s.seedDevices(ctx, projectID, indices); err != nil {
		return fmt.Errorf("failed to seed devices: %w", err)
	}
	if err := s.seedMerges(ctx, projectID, identifiedIDs); err != nil {
		return fmt.Errorf("failed to seed profile merges: %w", err)
	}
	return nil
}

// resolveProject finds the demo project in the org the demo user shares with
// the demo viewer, as the seeder created them together. The demo user's first
// org alone could be one created while its password was public, and taking it
// would send the demo's traffic there.
func (s *Seeder) resolveProject(ctx context.Context, read *dbread.Queries, customerID string) (dbread.Project, error) {
	orgs, err := read.GetOrgsByCustomerID(ctx, customerID)
	if err != nil {
		return dbread.Project{}, fmt.Errorf("failed to query orgs for customer %s: %w", customerID, err)
	}
	orgID, err := demoOrgID(ctx, read, orgs)
	if err != nil {
		return dbread.Project{}, err
	}
	projects, err := read.GetProjectsByOrgID(ctx, orgID)
	if err != nil {
		return dbread.Project{}, fmt.Errorf("failed to query projects for org %s: %w", orgID, err)
	}
	if len(projects) == 0 {
		// Deleting the demo project and restarting the worker rebuilds the demo.
		return s.createDemoProject(ctx, orgID)
	}
	return projects[0], nil
}

func (s *Seeder) createDemoProject(ctx context.Context, orgID string) (dbread.Project, error) {
	tx, err := s.deps.pg.Begin(ctx)
	if err != nil {
		return dbread.Project{}, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Demo profile ids are fixed and profiles.id is global, so the deleted
	// project's profiles would block the new one's until the purge erases them.
	for _, stmt := range []string{
		"DELETE FROM profile_devices WHERE project_id IN (SELECT id FROM projects WHERE org_id = $1 AND deletion_time IS NOT NULL)",
		"DELETE FROM profiles WHERE project_id IN (SELECT id FROM projects WHERE org_id = $1 AND deletion_time IS NOT NULL)",
	} {
		if _, err := tx.Exec(ctx, stmt, orgID); err != nil {
			return dbread.Project{}, fmt.Errorf("failed to clear deleted projects' profiles: %w", err)
		}
	}

	p, err := projects.CreateProjectInTx(ctx, dbwrite.New(tx), orgID, "default", "")
	if err != nil {
		return dbread.Project{}, fmt.Errorf("failed to create a project for org %s: %w", orgID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return dbread.Project{}, fmt.Errorf("failed to commit the new demo project: %w", err)
	}
	slog.InfoContext(ctx, "the demo org had no project, so the seeder created one",
		slog.String("org_id", orgID), slog.String("project_id", p.ID))
	return dbread.Project{ID: p.ID, OrgID: p.OrgID, DisplayName: p.DisplayName}, nil
}

// demoOrgID picks the first of the demo user's orgs, oldest first, that also has
// the demo viewer. A database seeded before the viewer existed has none, and
// nothing else marks the demo org, so it is refused rather than guessed at.
func demoOrgID(ctx context.Context, read *dbread.Queries, orgs []dbread.Org) (string, error) {
	const remedy = "a default `pug seed` recreates the demo data"
	viewer, err := read.GetCustomerByEmail(ctx, viewerEmail)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("there is no %s account to find the demo org by; %s", viewerEmail, remedy)
	}
	if err != nil {
		return "", fmt.Errorf("failed to look up the demo viewer: %w", err)
	}
	for _, org := range orgs {
		_, err := read.GetOrgMemberRole(ctx, dbread.GetOrgMemberRoleParams{OrgID: org.ID, CustomerID: viewer.ID})
		if err == nil {
			return org.ID, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("failed to check org %s for the demo viewer: %w", org.ID, err)
		}
	}
	return "", fmt.Errorf("no org has both %s and %s; %s", userEmail, viewerEmail, remedy)
}

// seedCustomerOrgProject creates the demo customer/org/project and returns the
// project alongside the raw private API key it minted for it. The project comes
// with a public key of its own; the private key is created explicitly (as any
// user would) so local dev has one to call the SDK/MCP endpoints with, and the
// raw value is returned because nothing but its digest is stored.
func (s *Seeder) seedCustomerOrgProject(ctx context.Context) (dbread.Project, string, error) {
	tx, err := s.deps.pg.Begin(ctx)
	if err != nil {
		return dbread.Project{}, "", fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	w := dbwrite.New(tx)

	// Stored as the org's admin so the org always has one on paper; the server
	// treats it as a viewer anyway.
	customer, err := w.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID:           xid.New().String(),
		Email:        userEmail,
		DisplayName:  userName,
		PasswordHash: "",
		PictureUri:   "",
	})
	if err != nil {
		return dbread.Project{}, "", fmt.Errorf("failed to create customer: %w", err)
	}

	org, err := w.CreateOrg(ctx, dbwrite.CreateOrgParams{
		ID:          xid.New().String(),
		DisplayName: "default",
	})
	if err != nil {
		return dbread.Project{}, "", fmt.Errorf("failed to create default org: %w", err)
	}

	if _, err = w.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID:      org.ID,
		CustomerID: customer.ID,
		Role:       coreorgs.RoleAdmin.String(),
	}); err != nil {
		return dbread.Project{}, "", fmt.Errorf("failed to add customer to org: %w", err)
	}

	// Snoop Pugg: a read-only companion on the same org so the viewer experience
	// is demoable out of the box, through DemoSignIn.
	viewer, err := w.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID:           xid.New().String(),
		Email:        viewerEmail,
		DisplayName:  viewerName,
		PasswordHash: "",
		PictureUri:   "",
	})
	if err != nil {
		return dbread.Project{}, "", fmt.Errorf("failed to create viewer customer: %w", err)
	}

	if _, err = w.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID:      org.ID,
		CustomerID: viewer.ID,
		Role:       coreorgs.RoleViewer.String(),
	}); err != nil {
		return dbread.Project{}, "", fmt.Errorf("failed to add viewer to org: %w", err)
	}

	p, err := projects.CreateProjectInTx(ctx, w, org.ID, "default", "")
	if err != nil {
		return dbread.Project{}, "", fmt.Errorf("failed to create project: %w", err)
	}

	privateKey, err := projects.CreateApiKeyInTx(ctx, w, p.ID, projects.KindPrivate, "seed")
	if err != nil {
		return dbread.Project{}, "", fmt.Errorf("failed to create private api key: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return dbread.Project{}, "", fmt.Errorf("failed to commit seed transaction: %w", err)
	}

	return dbread.Project{
		ID:          p.ID,
		OrgID:       p.OrgID,
		DisplayName: p.DisplayName,
	}, privateKey.RawKey, nil
}

// Customers of the Pug & Pals demo store are, naturally, dogs.
var firstNames = []string{
	"Biscuit", "Luna", "Max", "Bella", "Charlie", "Cooper", "Daisy", "Milo",
	"Rosie", "Teddy", "Winnie", "Ziggy", "Peanut", "Waffles", "Mochi",
	"Noodle", "Pickles", "Pepper", "Olive", "Hazel", "Gus", "Bruno",
	"Frankie", "Archie", "Poppy", "Maple", "Clover", "Scout", "Pretzel",
	"Banjo",
}

var lastNames = []string{
	"Barksdale", "Waggins", "Pawson", "McFluff", "Von Woof", "Sniffington",
	"Wigglesworth", "Beagleton", "Scruffins", "Fetcher", "Pugsley",
	"Furbanks", "Houndstooth", "Barkley", "Snoots", "Goodboy", "Droolittle",
	"Zoomies", "Borkman", "Floofington",
}

// Reserved .example TLD (RFC 6761) so demo emails can never resolve.
var emailDomains = []string{"barkmail.example", "woofhub.example", "fetchmail.example", "pugmail.example", "tailmail.example"}

var streetNames = []string{
	"Main St", "Oak Ave", "Maple Dr", "Park Blvd", "Cedar Ln",
	"Elm St", "Pine Rd", "Washington Ave", "Lake Dr", "Hill Ct",
}

// Weighted breeds: this is a pug company, the demo skews accordingly.
var breeds = []struct {
	name   string
	size   string
	weight int
}{
	{"Pug", "small", 12},
	{"Mixed (Best Kind)", "medium", 10},
	{"Golden Retriever", "large", 8},
	{"Labrador Retriever", "large", 8},
	{"French Bulldog", "small", 7},
	{"Corgi", "small", 6},
	{"Shiba Inu", "medium", 5},
	{"Beagle", "medium", 5},
	{"Dachshund", "small", 5},
	{"Border Collie", "medium", 4},
	{"Australian Shepherd", "medium", 4},
	{"Chihuahua", "small", 4},
	{"Siberian Husky", "large", 4},
	{"Pomeranian", "small", 3},
	{"Great Dane", "large", 2},
}

var favoriteTreats = []string{
	"Peanut Butter Training Bites", "Bully Sticks", "Sweet Potato Jerky",
	"Freeze-Dried Liver Treats", "Dental Chews", "Cheese (forbidden)",
	"Whatever the human is eating",
}

func pickBreedR(r *rand.Rand) (string, string) {
	total := 0
	for _, b := range breeds {
		total += b.weight
	}
	n := r.IntN(total)
	for _, b := range breeds {
		n -= b.weight
		if n < 0 {
			return b.name, b.size
		}
	}
	last := breeds[len(breeds)-1]
	return last.name, last.size
}

// profileSeed keys the deterministic per-user profile-property stream. Distinct
// from the event generator's userSeed so the two streams don't correlate, but
// like it, deterministic per index: the backfill seeder and the live worker
// derive byte-identical properties for the same user, so a live re-create of an
// already-seeded profile leaves its stored properties unchanged under the
// ReplacingMergeTree (only update_time advances).
const profileSeed = 0xC0FFEE

// DemoProfileProperties builds a dog profile aligned with the user's event data
// (same home city/country the event generator gives this distinct id, and
// pug_club membership matching the journeys the user runs) plus a deterministic
// identified/anonymous split. Returns the properties and the external id, which
// is "" for anonymous-only users, so the caller derives identified as
// externalID != "". Keyed only on i (the DemoUser is derived internally from the
// same index) so a mismatched (i, du) pair can't produce a Frankenstein profile.
// Deterministic in i so every caller agrees.
func DemoProfileProperties(i int) (props map[string]any, externalID string) {
	du := chseed.DemoUserAt(i)
	r := rand.New(rand.NewPCG(profileSeed, uint64(i)))
	first := firstNames[r.IntN(len(firstNames))]
	last := lastNames[r.IntN(len(lastNames))]
	breed, size := pickBreedR(r)

	props = map[string]any{
		"name":     fmt.Sprintf("%s %s", first, last),
		"breed":    breed,
		"dog_size": size,
		"city":     du.City,
		"country":  du.Country,
	}
	if du.Member {
		props["pug_club"] = true
	}

	// ~60% identified (signed up or signed in, which sets external_id), the rest
	// anonymous-only. Drawn before the optional rich fields so the split is
	// stable regardless of how many rich-field draws follow.
	if r.Float32() < 0.60 {
		externalID = externalIDForProfile(i)
	}

	// ~20% of profiles carry richer CRM-ish fields.
	if r.Float32() < 0.20 {
		props["first_name"] = first
		props["last_name"] = last
		props["favorite_treat"] = favoriteTreats[r.IntN(len(favoriteTreats))]
		props["age_years"] = 1 + r.IntN(12)

		if r.Float32() < 0.70 {
			props["email"] = fmt.Sprintf("%s.%s%d@%s",
				strings.ToLower(first),
				strings.ReplaceAll(strings.ToLower(last), " ", ""), i,
				emailDomains[r.IntN(len(emailDomains))],
			)
		}
		if r.Float32() < 0.30 {
			props["address"] = fmt.Sprintf("%d %s, %s",
				r.IntN(9900)+100,
				streetNames[r.IntN(len(streetNames))],
				du.City,
			)
		}
	}

	return props, externalID
}

// seedProfiles inserts a Postgres profile for each given user index, setting
// create_time to the user's join (their first-seen / anonymous-creation time,
// before identify) so profiles spread across the timeline. Returns the ids of
// the identified profiles for the merge-flow simulation.
func (s *Seeder) seedProfiles(ctx context.Context, projectID string, indices []int) ([]string, error) {
	slog.InfoContext(ctx, "seeding profiles",
		slog.String("project_id", projectID),
		slog.Int("count", len(indices)),
	)

	w := dbwrite.New(s.deps.pg)
	var identifiedIDs []string
	for _, i := range indices {
		id := fmt.Sprintf("user-%05d", i)
		du := chseed.DemoUserAt(i)
		props, externalID := DemoProfileProperties(i)

		if err := w.SeedDemoProfile(ctx, dbwrite.SeedDemoProfileParams{
			ID:         id,
			ProjectID:  projectID,
			ExternalID: dbtypes.NewOptionalText(externalID), // "" becomes NULL (anonymous)
			Properties: props,
			CreateTime: dbtypes.NewTimestamptz(du.Join),
			UpdateTime: dbtypes.NewTimestamptz(du.Join),
		}); err != nil {
			return nil, fmt.Errorf("seed profile %s: %w", id, err)
		}
		if externalID != "" {
			identifiedIDs = append(identifiedIDs, id)
		}
	}

	slog.InfoContext(ctx, "profiles seeded",
		slog.Int("count", len(indices)),
		slog.Int("identified", len(identifiedIDs)),
		slog.Int("anonymous", len(indices)-len(identifiedIDs)),
	)
	return identifiedIDs, nil
}

// seedMerges simulates the identify-time merge flow for ~30% of identified profiles.
// For each chosen profile, an anonymous profile is created with some properties,
// given a device, merged into the identified profile, devices reassigned, then deleted,
// mirroring the core merge steps from the identify worker (merge properties,
// reassign devices, delete source).
func (s *Seeder) seedMerges(ctx context.Context, projectID string, identifiedIDs []string) error {
	slog.InfoContext(ctx, "seeding profile merges", slog.Int("eligible", len(identifiedIDs)))

	w := dbwrite.New(s.deps.pg)
	merged := 0

	for _, targetID := range identifiedIDs {
		if rand.Float32() >= 0.30 {
			continue
		}

		anonID := xid.New().String()

		// Create the anonymous profile with minimal auto-properties
		if _, err := w.RegisterProfile(ctx, dbwrite.RegisterProfileParams{
			ID:        anonID,
			ProjectID: projectID,
			Properties: map[string]any{
				"$anonymous": "true",
			},
		}); err != nil {
			return fmt.Errorf("create anon profile %s: %w", anonID, err)
		}

		// Give the anon profile a device (simulates SDK-registered device pre-identify)
		platform := devicePlatforms[rand.IntN(len(devicePlatforms))]
		deviceID := xid.New().String()
		if _, err := w.SaveProfileDevice(ctx, dbwrite.SaveProfileDeviceParams{
			ID:         deviceID,
			Platform:   platform,
			ProfileID:  dbtypes.NewText(anonID),
			ProjectID:  projectID,
			Properties: map[string]any{},
			Status:     "active",
			Token:      randomPushToken(platform),
		}); err != nil {
			return fmt.Errorf("create anon device %s: %w", deviceID, err)
		}

		tx, err := s.deps.pg.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin merge tx: %w", err)
		}

		qtx := w.WithTx(tx)

		if _, err := qtx.MergeProfileProperties(ctx, dbwrite.MergeProfilePropertiesParams{
			SourceID:  anonID,
			TargetID:  targetID,
			ProjectID: projectID,
		}); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("merge %s into %s: %w", anonID, targetID, err)
		}

		if err := qtx.ReassignProfileDevices(ctx, dbwrite.ReassignProfileDevicesParams{
			SourceID:  dbtypes.NewText(anonID),
			TargetID:  dbtypes.NewText(targetID),
			ProjectID: projectID,
		}); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("reassign devices %s to %s: %w", anonID, targetID, err)
		}

		if _, err := qtx.SoftDeleteProfileByIDAndProjectID(ctx, dbwrite.SoftDeleteProfileByIDAndProjectIDParams{
			ID:        anonID,
			ProjectID: projectID,
		}); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("soft-delete anon profile %s: %w", anonID, err)
		}

		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit merge tx: %w", err)
		}

		merged++
	}

	slog.InfoContext(ctx, "profile merges seeded", slog.Int("merged", merged))
	return nil
}

var devicePlatforms = []string{"ios", "android", "web"}

func randomPushToken(platform string) string {
	switch platform {
	case "ios":
		// APNs tokens are 64 hex chars
		b := make([]byte, 32)
		for i := range b {
			b[i] = byte(rand.IntN(256))
		}
		return hex.EncodeToString(b)
	case "android":
		// FCM tokens are ~152 base64url chars; approximate with a random string
		const chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
		b := make([]byte, 152)
		for i := range b {
			b[i] = chars[rand.IntN(len(chars))]
		}
		return string(b)
	default:
		return ""
	}
}

// externalIDForProfile returns a unique external ID per seed index. We
// deliberately derive it from the index rather than the profile's email:
// UpsertProfileByExternalID conflicts on (project_id, external_id) and updates
// the existing row without creating the requested profile id, so a non-unique
// external_id would break the seeder's fixed user-%05d ids when attaching
// devices.
func externalIDForProfile(i int) string {
	return fmt.Sprintf("cust_%06d", i)
}

func (s *Seeder) seedDevices(ctx context.Context, projectID string, indices []int) error {
	slog.InfoContext(ctx, "seeding devices", slog.String("project_id", projectID))

	w := dbwrite.New(s.deps.pg)
	total := 0

	for _, i := range indices {
		profileID := fmt.Sprintf("user-%05d", i)
		// 1-3 devices per profile (~55% get 1, ~35% get 2, ~10% get 3)
		numDevices := 1
		r := rand.Float32()
		switch {
		case r < 0.10:
			numDevices = 3
		case r < 0.45:
			numDevices = 2
		}

		for d := range numDevices {
			platform := devicePlatforms[rand.IntN(len(devicePlatforms))]
			deviceID := fmt.Sprintf("dev-%05d-%d", i, d)
			token := randomPushToken(platform)
			status := "active"
			if rand.Float32() < 0.05 {
				status = "inactive"
			}

			if _, err := w.SaveProfileDevice(ctx, dbwrite.SaveProfileDeviceParams{
				ID:         deviceID,
				Platform:   platform,
				ProfileID:  dbtypes.NewText(profileID),
				ProjectID:  projectID,
				Properties: map[string]any{},
				Status:     status,
				Token:      token,
			}); err != nil {
				return fmt.Errorf("insert device %s: %w", deviceID, err)
			}
			total++
		}
	}

	slog.InfoContext(ctx, "devices seeded", slog.Int("count", total))
	return nil
}

// SeedAccount ensures the demo customer/org/project exists and returns it, using
// a caller-owned pool, WITHOUT seeding any profiles. Both the demo worker and
// the `pug seed` CLI derive the demo project from this and then seed profiles
// only for the users that produced events (SeedProfilesForUsers). With
// PUG_DEMO_ADMIN_PASSWORD set, it also creates the demo org's admin, or restores
// that account's role while its password matches.
func SeedAccount(ctx context.Context, pg *pgxpool.Pool) (dbread.Project, error) {
	password, err := AdminPasswordFromEnv()
	if err != nil {
		return dbread.Project{}, err
	}
	return NewSeeder(&deps{pg: pg, adminPassword: password}).seedAccount(ctx)
}

// AdminPasswordFromEnv reads PUG_DEMO_ADMIN_PASSWORD and checks it: at least 16
// characters, since the account's email is public, and at most bcrypt's 72
// bytes. Surrounding whitespace is refused too, since a secret file's trailing
// newline would otherwise become part of a password nobody knows. `pug seed`
// calls it before resetting anything. The error never includes the password.
func AdminPasswordFromEnv() (string, error) {
	p := os.Getenv("PUG_DEMO_ADMIN_PASSWORD")
	if p == "" {
		return "", nil
	}
	if strings.TrimSpace(p) != p {
		return "", errors.New("PUG_DEMO_ADMIN_PASSWORD has leading or trailing whitespace, such as a newline from a secret file")
	}
	if n := utf8.RuneCountInString(p); n < 16 || len(p) > 72 {
		return "", fmt.Errorf("PUG_DEMO_ADMIN_PASSWORD must be at least 16 characters and at most 72 bytes; it has %d characters in %d bytes", n, len(p))
	}
	return p, nil
}

// SeedProfilesForUsers seeds Postgres profiles, devices and merges for exactly
// the given user indices (the backfill's active set), using a caller-owned pool.
// A profile is created only for a user that has events.
func SeedProfilesForUsers(ctx context.Context, pg *pgxpool.Pool, projectID string, indices []int) error {
	return NewSeeder(&deps{pg: pg}).seedProfilesForUsers(ctx, projectID, indices)
}

// ResetDemoProfiles deletes all seeded profile rows (profiles, their devices, and
// the anonymous merge artifacts) for the demo project. It is the Postgres
// counterpart of the CLI's ClickHouse TRUNCATEs on the `seed --no-reset` path:
// without it the leftover Postgres profiles would trip seedProfilesForUsers'
// idempotency skip, so the freshly re-backfilled events would be paired with the
// stale profile set. profile_devices.profile_id is ON DELETE SET NULL (not
// cascade), so the devices are deleted explicitly; both deletes are scoped to the
// dedicated demo project_id, which covers user-%05d and xid-keyed merge rows.
func ResetDemoProfiles(ctx context.Context, pg *pgxpool.Pool, projectID string) error {
	for _, stmt := range []string{
		"DELETE FROM profile_devices WHERE project_id = $1",
		"DELETE FROM profiles WHERE project_id = $1",
	} {
		if _, err := pg.Exec(ctx, stmt, projectID); err != nil {
			return fmt.Errorf("reset demo profiles: %w", err)
		}
	}
	return nil
}

// CountDemoProfiles returns how many live (non-deleted) profiles exist for the
// demo project. The demo worker compares this against the ClickHouse profile
// count to detect a CopyProfilesToClickHouse that didn't finish (ClickHouse
// behind Postgres) and re-drive it. It counts every demo profile (the
// user-%05d rows and the xid-keyed merge artifacts) and excludes soft-deleted
// rows, matching exactly what the copy moves (GetAllProfilesByProjectID filters
// `deletion_time is null`), so a fully-copied project reads equal on both sides.
func CountDemoProfiles(ctx context.Context, pg *pgxpool.Pool, projectID string) (int64, error) {
	var n int64
	if err := pg.QueryRow(ctx,
		"SELECT count(*) FROM profiles WHERE project_id = $1 AND deletion_time IS NULL", projectID,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("count demo profiles: %w", err)
	}
	return n, nil
}
