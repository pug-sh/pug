package seed

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	chseed "github.com/pug-sh/pug/internal/app/seed/clickhouse"
	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	"github.com/pug-sh/pug/internal/core/projects"
	dbtypes "github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/rs/xid"
	"golang.org/x/crypto/bcrypt"
)

const (
	testEmail    = "woof@pug.sh"
	testPassword = "goodboy"
	testName     = "Pug"
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
	read := dbread.New(s.deps.pg)

	slog.InfoContext(ctx, "checking for existing test user")

	customer, err := read.GetCustomerByEmail(ctx, testEmail)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return dbread.Project{}, fmt.Errorf("failed to check existing user: %w", err)
	}
	if err == nil {
		slog.InfoContext(ctx, "test user already exists, resolving project")
		return s.resolveProject(ctx, read, customer.ID)
	}

	slog.InfoContext(ctx, "creating test user", slog.String("email", testEmail))
	project, err := s.seedCustomerOrgProject(ctx)
	if err != nil {
		return dbread.Project{}, err
	}
	slog.DebugContext(ctx, "account seed complete",
		slog.String("project_id", project.ID),
		slog.String("public_api_key", project.PublicApiKey),
		slog.String("private_api_key", project.PrivateApiKey),
	)
	return project, nil
}

// seedProfilesForUsers seeds Postgres profiles, devices and merges for exactly
// the given user indices (parsed from the backfill's emitted distinct ids), so a
// profile exists only for a user with events.
func (s *Seeder) seedProfilesForUsers(ctx context.Context, projectID string, indices []int) error {
	// Idempotency: skip if this project already has seeded profiles. seedProfiles
	// and seedDevices upsert, but seedMerges mints fresh xid-keyed anonymous rows
	// every run, so re-seeding a populated project would accumulate junk. The
	// worker never hits this (it seeds once, gated on an empty event count); the
	// CLI's `seed --no-reset` path clears the demo rows first (ResetDemoProfiles),
	// so it falls through to a real re-seed rather than relying on this skip.
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

func (s *Seeder) resolveProject(ctx context.Context, read *dbread.Queries, customerID string) (dbread.Project, error) {
	orgs, err := read.GetOrgsByCustomerID(ctx, customerID)
	if err != nil {
		return dbread.Project{}, fmt.Errorf("failed to query orgs for customer %s: %w", customerID, err)
	}
	if len(orgs) == 0 {
		return dbread.Project{}, fmt.Errorf("no orgs found for customer %s", customerID)
	}
	projects, err := read.GetProjectsByOrgID(ctx, orgs[0].ID)
	if err != nil {
		return dbread.Project{}, fmt.Errorf("failed to query projects for org %s: %w", orgs[0].ID, err)
	}
	if len(projects) == 0 {
		return dbread.Project{}, fmt.Errorf("no projects found for org %s", orgs[0].ID)
	}
	return projects[0], nil
}

func (s *Seeder) seedCustomerOrgProject(ctx context.Context) (dbread.Project, error) {
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.DefaultCost)
	if err != nil {
		return dbread.Project{}, fmt.Errorf("failed to hash password: %w", err)
	}

	privKey, err := projects.NewPrivateKey()
	if err != nil {
		return dbread.Project{}, fmt.Errorf("failed to generate private api key: %w", err)
	}

	pubKey, err := projects.NewPublicKey()
	if err != nil {
		return dbread.Project{}, fmt.Errorf("failed to generate public api key: %w", err)
	}

	tx, err := s.deps.pg.Begin(ctx)
	if err != nil {
		return dbread.Project{}, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	w := dbwrite.New(tx)

	customer, err := w.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID:           xid.New().String(),
		Email:        testEmail,
		DisplayName:  testName,
		PasswordHash: string(passwordHash),
		PictureUri:   "",
	})
	if err != nil {
		return dbread.Project{}, fmt.Errorf("failed to create customer: %w", err)
	}

	org, err := w.CreateOrg(ctx, dbwrite.CreateOrgParams{
		ID:          xid.New().String(),
		DisplayName: "default",
	})
	if err != nil {
		return dbread.Project{}, fmt.Errorf("failed to create default org: %w", err)
	}

	if _, err = w.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID:      org.ID,
		CustomerID: customer.ID,
		Role:       coreorgs.RoleAdmin.String(),
	}); err != nil {
		return dbread.Project{}, fmt.Errorf("failed to add customer to org: %w", err)
	}

	p, err := w.CreateProject(ctx, dbwrite.CreateProjectParams{
		ID:            xid.New().String(),
		OrgID:         org.ID,
		DisplayName:   "default",
		PrivateApiKey: privKey,
		PublicApiKey:  pubKey,
	})
	if err != nil {
		return dbread.Project{}, fmt.Errorf("failed to create project: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return dbread.Project{}, fmt.Errorf("failed to commit seed transaction: %w", err)
	}

	return dbread.Project{
		ID:            p.ID,
		OrgID:         p.OrgID,
		DisplayName:   p.DisplayName,
		PrivateApiKey: p.PrivateApiKey,
		PublicApiKey:  p.PublicApiKey,
	}, nil
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

// Weighted breeds — this is a pug company, the demo skews accordingly.
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
// is "" for anonymous-only users — so the caller derives identified as
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

	// ~60% identified (signed up / signed in → external_id), the rest
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
			ExternalID: dbtypes.NewOptionalText(externalID), // "" → NULL (anonymous)
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
// given a device, merged into the identified profile, devices reassigned, then deleted —
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
			return fmt.Errorf("merge %s → %s: %w", anonID, targetID, err)
		}

		if err := qtx.ReassignProfileDevices(ctx, dbwrite.ReassignProfileDevicesParams{
			SourceID:  dbtypes.NewText(anonID),
			TargetID:  dbtypes.NewText(targetID),
			ProjectID: projectID,
		}); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("reassign devices %s → %s: %w", anonID, targetID, err)
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
		return fmt.Sprintf("%x", b)
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
// only for the users that produced events (SeedProfilesForUsers).
func SeedAccount(ctx context.Context, pg *pgxpool.Pool) (dbread.Project, error) {
	return NewSeeder(&deps{pg: pg}).seedAccount(ctx)
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
