package seed

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"

	"github.com/fivebitsio/cotton/internal/core/projects"
	orgsv1 "github.com/fivebitsio/cotton/internal/gen/proto/orgs/v1"
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

	privKey, err := projects.NewPrivateKey()
	if err != nil {
		return fmt.Errorf("failed to generate private api key: %w", err)
	}

	pubKey, err := projects.NewPublicKey()
	if err != nil {
		return fmt.Errorf("failed to generate public api key: %w", err)
	}

	tx, err := s.deps.pg.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
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
		return fmt.Errorf("failed to create customer: %w", err)
	}

	org, err := w.CreateOrg(ctx, dbwrite.CreateOrgParams{
		ID:          xid.New().String(),
		DisplayName: "default",
	})
	if err != nil {
		return fmt.Errorf("failed to create default org: %w", err)
	}

	if _, err = w.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID:      org.ID,
		CustomerID: customer.ID,
		Role:       orgsv1.OrgRole_ORG_ROLE_ADMIN.String(),
	}); err != nil {
		return fmt.Errorf("failed to add customer to org: %w", err)
	}

	project, err := w.CreateProject(ctx, dbwrite.CreateProjectParams{
		ID:            xid.New().String(),
		OrgID:         org.ID,
		DisplayName:   "default",
		PrivateApiKey: privKey,
		PublicApiKey:  pubKey,
	})
	if err != nil {
		return fmt.Errorf("failed to create project: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("failed to commit seed transaction: %w", err)
	}

	identifiedIDs, err := s.seedProfiles(ctx, project.ID)
	if err != nil {
		return fmt.Errorf("failed to seed profiles: %w", err)
	}

	if err := s.seedDevices(ctx, project.ID); err != nil {
		return fmt.Errorf("failed to seed devices: %w", err)
	}

	if err := s.seedMerges(ctx, project.ID, identifiedIDs); err != nil {
		return fmt.Errorf("failed to seed profile merges: %w", err)
	}

	slog.DebugContext(ctx, "seed complete",
		slog.String("customer_id", customer.ID),
		slog.String("org_id", org.ID),
		slog.String("project_id", project.ID),
		slog.String("public_api_key", project.PublicApiKey),
		slog.String("private_api_key", project.PrivateApiKey),
	)

	return nil
}

const profileCount = 10_000

var firstNames = []string{
	"Alice", "Bob", "Carlos", "Diana", "Emma", "Felix", "Grace", "Henry",
	"Isabel", "James", "Karen", "Liam", "Mia", "Noah", "Olivia", "Paul",
	"Quinn", "Rachel", "Sam", "Tina", "Uma", "Victor", "Wendy", "Xander",
	"Yara", "Zoe",
}

var lastNames = []string{
	"Smith", "Johnson", "Williams", "Brown", "Jones", "Garcia", "Miller",
	"Davis", "Wilson", "Moore", "Taylor", "Anderson", "Thomas", "Jackson",
	"White", "Harris", "Martin", "Thompson", "Young", "Lee",
}

var emailDomains = []string{"gmail.com", "yahoo.com", "outlook.com", "icloud.com", "proton.me"}

var streetNames = []string{
	"Main St", "Oak Ave", "Maple Dr", "Park Blvd", "Cedar Ln",
	"Elm St", "Pine Rd", "Washington Ave", "Lake Dr", "Hill Ct",
}

var cities = []string{
	"New York", "Los Angeles", "Chicago", "Houston", "Phoenix",
	"Philadelphia", "San Antonio", "San Diego", "Dallas", "Austin",
}

func randomProperties(i int) map[string]any {
	first := firstNames[rand.IntN(len(firstNames))]
	last := lastNames[rand.IntN(len(lastNames))]

	// ~80% of profiles just have name; ~20% have richer fields
	if rand.Float32() < 0.80 {
		return map[string]any{
			"name": fmt.Sprintf("%s %s", first, last),
		}
	}

	props := map[string]any{
		"first_name": first,
		"last_name":  last,
	}

	if rand.Float32() < 0.70 {
		props["email"] = fmt.Sprintf("%s.%s%d@%s",
			lowerASCII(first), lowerASCII(last), i%1000,
			emailDomains[rand.IntN(len(emailDomains))],
		)
	}
	if rand.Float32() < 0.50 {
		props["phone"] = fmt.Sprintf("+1%03d%03d%04d",
			rand.IntN(800)+100, rand.IntN(900)+100, rand.IntN(10000))
	}
	if rand.Float32() < 0.30 {
		props["address"] = fmt.Sprintf("%d %s, %s",
			rand.IntN(9900)+100,
			streetNames[rand.IntN(len(streetNames))],
			cities[rand.IntN(len(cities))],
		)
	}

	return props
}

// lowerASCII lowercases ASCII letters only (avoids unicode import for a simple seed helper).
func lowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

func (s *Seeder) seedProfiles(ctx context.Context, projectID string) ([]string, error) {
	slog.InfoContext(ctx, "seeding profiles",
		slog.String("project_id", projectID),
		slog.Int("count", profileCount),
	)

	w := dbwrite.New(s.deps.pg)
	var identifiedIDs []string
	for i := range profileCount {
		id := fmt.Sprintf("user-%05d", i)
		props := randomProperties(i)
		if _, err := w.RegisterProfile(ctx, dbwrite.RegisterProfileParams{
			ID:         id,
			ProjectID:  projectID,
			Properties: props,
		}); err != nil {
			return nil, fmt.Errorf("insert profile %s: %w", id, err)
		}

		// ~60% of profiles are identified — they have an external_id set,
		// matching what an identify() call from the SDK would produce.
		// Use email from properties if present, otherwise a numeric customer ID.
		if rand.Float32() < 0.60 {
			externalID := externalIDForProfile(props, i)
			if _, err := w.SetProfileExternalID(ctx, dbwrite.SetProfileExternalIDParams{
				ID:         id,
				ProjectID:  projectID,
				ExternalID: externalID,
			}); err != nil {
				return nil, fmt.Errorf("set external_id for profile %s: %w", id, err)
			}
			identifiedIDs = append(identifiedIDs, id)
		}
	}

	slog.InfoContext(ctx, "profiles seeded",
		slog.Int("count", profileCount),
		slog.Int("identified", len(identifiedIDs)),
		slog.Int("anonymous", profileCount-len(identifiedIDs)),
	)
	return identifiedIDs, nil
}

// seedMerges simulates the identify-time merge flow for ~30% of identified profiles.
// For each chosen profile, an anonymous profile is created with some properties,
// given a device, merged into the identified profile, devices reassigned, then deleted —
// matching exactly what the identify worker does in production.
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
			ProfileID:  anonID,
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
			SourceID:  anonID,
			TargetID:  targetID,
			ProjectID: projectID,
		}); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("reassign devices %s → %s: %w", anonID, targetID, err)
		}

		if _, err := qtx.DeleteProfileByIDAndProjectID(ctx, dbwrite.DeleteProfileByIDAndProjectIDParams{
			ID:        anonID,
			ProjectID: projectID,
		}); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("delete anon profile %s: %w", anonID, err)
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

// externalIDForProfile returns an external ID for an identified profile.
// If the profile already has an email property, reuse it — otherwise use a
// numeric customer ID, matching what a typical identify() call looks like.
func externalIDForProfile(props map[string]any, i int) string {
	if email, ok := props["email"].(string); ok && email != "" {
		return email
	}
	return fmt.Sprintf("cust_%06d", i)
}

func (s *Seeder) seedDevices(ctx context.Context, projectID string) error {
	slog.InfoContext(ctx, "seeding devices", slog.String("project_id", projectID))

	w := dbwrite.New(s.deps.pg)
	total := 0

	for i := range profileCount {
		profileID := fmt.Sprintf("user-%05d", i)
		// 1-3 devices per profile, weighted toward 1
		numDevices := 1
		if rand.Float32() < 0.35 {
			numDevices = 2
		} else if rand.Float32() < 0.10 {
			numDevices = 3
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
				ProfileID:  profileID,
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

func Run(ctx context.Context) error {
	d, err := newDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	return NewSeeder(d).Run(ctx)
}
