package devices

import (
	"context"
	"testing"

	devicesv1 "github.com/fivebitsio/cotton/internal/gen/proto/sdk/devices/v1"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/fivebitsio/cotton/internal/testutil"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/rs/xid"
	"google.golang.org/protobuf/proto"
)

// seedProject inserts an org and project into the test database, returning the project ID.
func seedProject(t *testing.T, ctx context.Context, pg *testutil.TestPostgres) string {
	t.Helper()
	orgID := xid.New().String()
	projectID := xid.New().String()
	if _, err := pg.PgW.Exec(ctx,
		`INSERT INTO orgs (id, display_name) VALUES ($1, $2)`,
		orgID, "test-org"); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	if _, err := pg.PgW.Exec(ctx,
		`INSERT INTO projects (id, org_id, display_name, private_api_key, public_api_key) VALUES ($1, $2, $3, $4, $5)`,
		projectID, orgID, "test-project",
		xid.New().String()+"test",
		xid.New().String()+"test",
	); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	return projectID
}

// makeSubscribeData marshals a DeviceOperationMessage with a Subscribe payload.
func makeSubscribeData(t *testing.T, deviceID, projectID, platform, token, profileExternalID string) []byte {
	t.Helper()
	msg := &devicesv1.DeviceOperationMessage{
		DeviceId:  deviceID,
		ProjectId: projectID,
		OperationPayload: &devicesv1.DeviceOperationMessage_Subscribe{
			Subscribe: &devicesv1.SubscribePayload{
				Platform:          platform,
				Token:             token,
				ProfileExternalId: profileExternalID,
			},
		},
	}
	data, err := proto.Marshal(msg)
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}
	return data
}

func TestHandleSubscribe_Anonymous(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	pg := testutil.SetupPostgres(t)
	projectID := seedProject(t, ctx, pg)

	worker := NewWorker(pg.PgW, pg.PgW)

	deviceID := xid.New().String() // 20 chars
	data := makeSubscribeData(t, deviceID, projectID, "ios", "test-token", "")

	if err := worker.ProcessMessage(ctx, data); err != nil {
		t.Fatalf("ProcessMessage: %v", err)
	}

	// Verify the device was saved with a NULL profile_id.
	var profileID pgtype.Text
	err := pg.PgW.QueryRow(ctx,
		`SELECT profile_id FROM profile_devices WHERE id = $1 AND project_id = $2`,
		deviceID, projectID,
	).Scan(&profileID)
	if err != nil {
		t.Fatalf("query device: %v", err)
	}
	if profileID.Valid {
		t.Errorf("profile_id = %q, want NULL for anonymous device", profileID.String)
	}
}

func TestHandleSubscribe_RelinkOnResubscribe(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	pg := testutil.SetupPostgres(t)
	projectID := seedProject(t, ctx, pg)

	worker := NewWorker(pg.PgW, pg.PgW)

	deviceID := xid.New().String()

	// Step 1: Subscribe anonymously — no profile.
	anonData := makeSubscribeData(t, deviceID, projectID, "ios", "test-token", "")
	if err := worker.ProcessMessage(ctx, anonData); err != nil {
		t.Fatalf("anonymous ProcessMessage: %v", err)
	}

	// Verify NULL profile_id.
	var profileID pgtype.Text
	err := pg.PgW.QueryRow(ctx,
		`SELECT profile_id FROM profile_devices WHERE id = $1 AND project_id = $2`,
		deviceID, projectID,
	).Scan(&profileID)
	if err != nil {
		t.Fatalf("query device after anonymous subscribe: %v", err)
	}
	if profileID.Valid {
		t.Fatalf("expected NULL profile_id after anonymous subscribe, got %q", profileID.String)
	}

	// Step 2: Create an identified profile.
	write := dbwrite.New(pg.PgW)
	identifiedID := xid.New().String()
	profile, err := write.UpsertProfileByExternalID(ctx, dbwrite.UpsertProfileByExternalIDParams{
		ID:         identifiedID,
		ProjectID:  projectID,
		ExternalID: pgtype.Text{String: "alice@test.com", Valid: true},
		Properties: map[string]any{"plan": "pro"},
	})
	if err != nil {
		t.Fatalf("upsert profile: %v", err)
	}

	// Step 3: Re-subscribe the same device with a profile_external_id.
	resubData := makeSubscribeData(t, deviceID, projectID, "ios", "test-token", "alice@test.com")
	if err := worker.ProcessMessage(ctx, resubData); err != nil {
		t.Fatalf("re-subscribe ProcessMessage: %v", err)
	}

	// Verify profile_id is now set to the identified profile.
	var linkedProfileID pgtype.Text
	err = pg.PgW.QueryRow(ctx,
		`SELECT profile_id FROM profile_devices WHERE id = $1 AND project_id = $2`,
		deviceID, projectID,
	).Scan(&linkedProfileID)
	if err != nil {
		t.Fatalf("query device after re-subscribe: %v", err)
	}
	if !linkedProfileID.Valid {
		t.Fatal("profile_id is NULL after re-subscribe, want linked profile ID")
	}
	if linkedProfileID.String != profile.ID {
		t.Errorf("profile_id = %q, want %q", linkedProfileID.String, profile.ID)
	}
}

func TestHandleSubscribe_AnonymousDoesNotUnlinkExistingDevice(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	pg := testutil.SetupPostgres(t)
	projectID := seedProject(t, ctx, pg)

	worker := NewWorker(pg.PgW, pg.PgW)

	// Create a profile.
	write := dbwrite.New(pg.PgW)
	profileID := xid.New().String()
	profile, err := write.UpsertProfileByExternalID(ctx, dbwrite.UpsertProfileByExternalIDParams{
		ID:         profileID,
		ProjectID:  projectID,
		ExternalID: pgtype.Text{String: "linked@test.com", Valid: true},
		Properties: map[string]any{},
	})
	if err != nil {
		t.Fatalf("upsert profile: %v", err)
	}

	// Register device linked to the profile.
	deviceID := xid.New().String()
	if _, err := write.SaveProfileDevice(ctx, dbwrite.SaveProfileDeviceParams{
		ID:         deviceID,
		Platform:   "ios",
		ProfileID:  pgtype.Text{String: profile.ID, Valid: true},
		ProjectID:  projectID,
		Properties: map[string]any{},
		Status:     "active",
		Token:      "existing-token",
	}); err != nil {
		t.Fatalf("save linked device: %v", err)
	}

	// Anonymous re-subscribe with the same device_id and NO profile_external_id.
	anonData := makeSubscribeData(t, deviceID, projectID, "ios", "existing-token", "")
	if err := worker.ProcessMessage(ctx, anonData); err != nil {
		t.Fatalf("anonymous re-subscribe ProcessMessage: %v", err)
	}

	// Verify profile_id is STILL set (coalesce prevents NULL overwrite).
	var linkedProfileID pgtype.Text
	if err := pg.PgW.QueryRow(ctx,
		`SELECT profile_id FROM profile_devices WHERE id = $1 AND project_id = $2`,
		deviceID, projectID,
	).Scan(&linkedProfileID); err != nil {
		t.Fatalf("query device after anonymous re-subscribe: %v", err)
	}
	if !linkedProfileID.Valid {
		t.Fatal("profile_id is NULL after anonymous re-subscribe, want existing profile preserved")
	}
	if linkedProfileID.String != profile.ID {
		t.Errorf("profile_id = %q, want %q (original link preserved)", linkedProfileID.String, profile.ID)
	}
}

func TestHandleSubscribe_WithProfile(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	pg := testutil.SetupPostgres(t)
	projectID := seedProject(t, ctx, pg)

	worker := NewWorker(pg.PgW, pg.PgW)

	// Create a profile.
	write := dbwrite.New(pg.PgW)
	profileID := xid.New().String()
	profile, err := write.UpsertProfileByExternalID(ctx, dbwrite.UpsertProfileByExternalIDParams{
		ID:         profileID,
		ProjectID:  projectID,
		ExternalID: pgtype.Text{String: "bob@test.com", Valid: true},
		Properties: map[string]any{},
	})
	if err != nil {
		t.Fatalf("upsert profile: %v", err)
	}

	// Subscribe device with profile_external_id.
	deviceID := xid.New().String()
	data := makeSubscribeData(t, deviceID, projectID, "android", "device-token-xyz", "bob@test.com")
	if err := worker.ProcessMessage(ctx, data); err != nil {
		t.Fatalf("ProcessMessage: %v", err)
	}

	// Verify the device was saved with the correct profile_id.
	var linkedProfileID pgtype.Text
	err = pg.PgW.QueryRow(ctx,
		`SELECT profile_id FROM profile_devices WHERE id = $1 AND project_id = $2`,
		deviceID, projectID,
	).Scan(&linkedProfileID)
	if err != nil {
		t.Fatalf("query device: %v", err)
	}
	if !linkedProfileID.Valid {
		t.Fatal("profile_id is NULL, want linked profile")
	}
	if linkedProfileID.String != profile.ID {
		t.Errorf("profile_id = %q, want %q", linkedProfileID.String, profile.ID)
	}
}
