package identify

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fivebitsio/cotton/internal/app/workers/profiles"
	natsworker "github.com/fivebitsio/cotton/internal/deps/nats"
	"github.com/fivebitsio/cotton/internal/deps/postgres"
	sdkprofilesv1 "github.com/fivebitsio/cotton/internal/gen/proto/sdk/profiles/v1"
	workerprofilesv1 "github.com/fivebitsio/cotton/internal/gen/proto/workers/profiles/v1"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/fivebitsio/cotton/internal/testutil"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/rs/xid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// --- helpers ---

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

func setupNATSClient(t *testing.T, ctx context.Context) (*natsworker.NATSClient, jetstream.JetStream) {
	t.Helper()
	tn := testutil.SetupNATS(t)
	t.Setenv("NATS_URL", tn.URL)

	client, err := natsworker.New(ctx)
	if err != nil {
		t.Fatalf("natsworker.New: %v", err)
	}
	t.Cleanup(client.Close)

	// Create a stream covering all profile subjects.
	nc, err := nats.Connect(tn.URL)
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	t.Cleanup(nc.Close)

	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream.New: %v", err)
	}
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "profiles",
		Subjects: []string{"profiles.>"},
	}); err != nil {
		t.Fatalf("create stream: %v", err)
	}

	return client, js
}

func makeIdentifyData(t *testing.T, projectID, externalID, anonymousID, deviceID string, traits map[string]any) []byte {
	t.Helper()
	var traitsPB *structpb.Struct
	if traits != nil {
		var err error
		traitsPB, err = structpb.NewStruct(traits)
		if err != nil {
			t.Fatalf("structpb.NewStruct: %v", err)
		}
	}
	msg := &sdkprofilesv1.ProfileIdentifyMessage{
		ProjectId:   projectID,
		ExternalId:  externalID,
		AnonymousId: anonymousID,
		DeviceId:    deviceID,
		Traits:      traitsPB,
	}
	data, err := proto.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

// fetchUpserts consumes up to n messages from the profiles.upsert subject.
func fetchUpserts(t *testing.T, ctx context.Context, js jetstream.JetStream, n int) []*workerprofilesv1.ProfileUpsertMessage {
	t.Helper()
	cons, err := js.CreateOrUpdateConsumer(ctx, "profiles", jetstream.ConsumerConfig{
		FilterSubject: natsworker.ProfileUpsertSubject,
		Name:          "test-upsert-" + xid.New().String(),
	})
	if err != nil {
		t.Fatalf("create upsert consumer: %v", err)
	}
	batch, err := cons.Fetch(n, jetstream.FetchMaxWait(2_000_000_000))
	if err != nil {
		t.Fatalf("fetch upserts: %v", err)
	}
	var msgs []*workerprofilesv1.ProfileUpsertMessage
	for msg := range batch.Messages() {
		var m workerprofilesv1.ProfileUpsertMessage
		if err := proto.Unmarshal(msg.Data(), &m); err != nil {
			t.Fatalf("unmarshal upsert: %v", err)
		}
		msgs = append(msgs, &m)
	}
	return msgs
}

// fetchAliases consumes up to n messages from the profiles.alias subject.
func fetchAliases(t *testing.T, ctx context.Context, js jetstream.JetStream, n int) []*workerprofilesv1.ProfileAliasMessage {
	t.Helper()
	cons, err := js.CreateOrUpdateConsumer(ctx, "profiles", jetstream.ConsumerConfig{
		FilterSubject: natsworker.ProfileAliasSubject,
		Name:          "test-alias-" + xid.New().String(),
	})
	if err != nil {
		t.Fatalf("create alias consumer: %v", err)
	}
	batch, err := cons.Fetch(n, jetstream.FetchMaxWait(2_000_000_000))
	if err != nil {
		t.Fatalf("fetch aliases: %v", err)
	}
	var msgs []*workerprofilesv1.ProfileAliasMessage
	for msg := range batch.Messages() {
		var m workerprofilesv1.ProfileAliasMessage
		if err := proto.Unmarshal(msg.Data(), &m); err != nil {
			t.Fatalf("unmarshal alias: %v", err)
		}
		msgs = append(msgs, &m)
	}
	return msgs
}

// --- tests ---

func TestHandleIdentify_UpsertOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	pg := testutil.SetupPostgres(t)
	projectID := seedProject(t, ctx, pg)
	natsClient, js := setupNATSClient(t, ctx)

	w := profiles.NewWorker(pg.PgW)

	data := makeIdentifyData(t, projectID, "alice@example.com", "", "", map[string]any{"plan": "pro"})
	if err := handleIdentify(ctx, w, natsClient, data); err != nil {
		t.Fatalf("handleIdentify: %v", err)
	}

	// Verify profile was created in Postgres.
	var profileID, extID string
	var props map[string]any
	err := pg.PgW.QueryRow(ctx,
		`SELECT id, external_id, properties FROM profiles WHERE project_id = $1 AND external_id = $2`,
		projectID, "alice@example.com",
	).Scan(&profileID, &extID, &props)
	if err != nil {
		t.Fatalf("query profile: %v", err)
	}
	if extID != "alice@example.com" {
		t.Errorf("external_id = %q, want %q", extID, "alice@example.com")
	}
	if props["plan"] != "pro" {
		t.Errorf("properties.plan = %v, want %q", props["plan"], "pro")
	}

	// Verify exactly 1 NATS upsert message.
	upserts := fetchUpserts(t, ctx, js, 1)
	if len(upserts) != 1 {
		t.Fatalf("expected 1 upsert, got %d", len(upserts))
	}
	if upserts[0].ProfileId != profileID {
		t.Errorf("upsert.ProfileId = %q, want %q", upserts[0].ProfileId, profileID)
	}
	if upserts[0].ExternalId != "alice@example.com" {
		t.Errorf("upsert.ExternalId = %q, want %q", upserts[0].ExternalId, "alice@example.com")
	}
	if upserts[0].IsDeleted {
		t.Error("upsert.IsDeleted = true, want false")
	}
}

func TestHandleIdentify_UpsertMergesTraits(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	pg := testutil.SetupPostgres(t)
	projectID := seedProject(t, ctx, pg)
	natsClient, _ := setupNATSClient(t, ctx)

	w := profiles.NewWorker(pg.PgW)

	// First identify: creates profile.
	data1 := makeIdentifyData(t, projectID, "bob@test.com", "", "", map[string]any{"plan": "free"})
	if err := handleIdentify(ctx, w, natsClient, data1); err != nil {
		t.Fatalf("first identify: %v", err)
	}

	// Capture profile ID after first identify.
	var firstID string
	err := pg.PgW.QueryRow(ctx,
		`SELECT id FROM profiles WHERE project_id = $1 AND external_id = $2`,
		projectID, "bob@test.com",
	).Scan(&firstID)
	if err != nil {
		t.Fatalf("query profile after first identify: %v", err)
	}

	// Second identify: merges traits — new traits overwrite existing keys.
	data2 := makeIdentifyData(t, projectID, "bob@test.com", "", "", map[string]any{"plan": "pro", "role": "admin"})
	if err := handleIdentify(ctx, w, natsClient, data2); err != nil {
		t.Fatalf("second identify: %v", err)
	}

	var secondID string
	var props map[string]any
	err = pg.PgW.QueryRow(ctx,
		`SELECT id, properties FROM profiles WHERE project_id = $1 AND external_id = $2`,
		projectID, "bob@test.com",
	).Scan(&secondID, &props)
	if err != nil {
		t.Fatalf("query profile: %v", err)
	}
	if secondID != firstID {
		t.Errorf("profile ID changed across upserts: %q → %q", firstID, secondID)
	}
	if props["plan"] != "pro" {
		t.Errorf("plan = %v, want %q (new traits should overwrite)", props["plan"], "pro")
	}
	if props["role"] != "admin" {
		t.Errorf("role = %v, want %q", props["role"], "admin")
	}
}

func TestHandleIdentify_MergeAnonymous(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	pg := testutil.SetupPostgres(t)
	projectID := seedProject(t, ctx, pg)
	natsClient, js := setupNATSClient(t, ctx)

	write := dbwrite.New(pg.PgW)

	// Create an anonymous profile with a device.
	anonID := xid.New().String()
	if _, err := write.RegisterProfile(ctx, dbwrite.RegisterProfileParams{
		ID:         anonID,
		ProjectID:  projectID,
		Properties: map[string]any{"device_type": "mobile"},
	}); err != nil {
		t.Fatalf("register anon profile: %v", err)
	}

	deviceID := xid.New().String()
	if _, err := write.SaveProfileDevice(ctx, dbwrite.SaveProfileDeviceParams{
		ID:         deviceID,
		Platform:   "ios",
		ProfileID:  postgres.NewText(anonID),
		ProjectID:  projectID,
		Properties: map[string]any{},
		Status:     "active",
		Token:      "test-token-abc",
	}); err != nil {
		t.Fatalf("save device: %v", err)
	}

	w := profiles.NewWorker(pg.PgW)

	// Identify with anonymous_id → triggers merge.
	data := makeIdentifyData(t, projectID, "carol@test.com", anonID, "", map[string]any{"plan": "enterprise"})
	if err := handleIdentify(ctx, w, natsClient, data); err != nil {
		t.Fatalf("handleIdentify: %v", err)
	}

	// Verify: identified profile exists with merged properties.
	var targetID string
	var props map[string]any
	err := pg.PgW.QueryRow(ctx,
		`SELECT id, properties FROM profiles WHERE project_id = $1 AND external_id = $2`,
		projectID, "carol@test.com",
	).Scan(&targetID, &props)
	if err != nil {
		t.Fatalf("query target profile: %v", err)
	}
	if props["plan"] != "enterprise" {
		t.Errorf("plan = %v, want %q", props["plan"], "enterprise")
	}

	// Verify: anonymous profile is soft-deleted (row exists but deletion_time is set).
	var deletedAt *string
	err = pg.PgW.QueryRow(ctx,
		`SELECT deletion_time::text FROM profiles WHERE id = $1 AND project_id = $2`,
		anonID, projectID,
	).Scan(&deletedAt)
	if err != nil {
		t.Fatalf("query anon deletion_time: %v", err)
	}
	if deletedAt == nil {
		t.Errorf("anonymous profile deletion_time is null, want soft-deleted")
	}

	// Verify: device was reassigned to target.
	var deviceProfileID string
	err = pg.PgW.QueryRow(ctx,
		`SELECT profile_id FROM profile_devices WHERE id = $1`,
		deviceID,
	).Scan(&deviceProfileID)
	if err != nil {
		t.Fatalf("query device: %v", err)
	}
	if deviceProfileID != targetID {
		t.Errorf("device.profile_id = %q, want %q (target)", deviceProfileID, targetID)
	}

	// Verify NATS: 2 upserts (target + soft-delete) + 1 alias.
	upserts := fetchUpserts(t, ctx, js, 2)
	if len(upserts) != 2 {
		t.Fatalf("expected 2 upserts, got %d", len(upserts))
	}
	if upserts[0].ProfileId != targetID {
		t.Errorf("target upsert ProfileId = %q, want %q", upserts[0].ProfileId, targetID)
	}
	if upserts[0].IsDeleted {
		t.Error("target upsert IsDeleted = true, want false")
	}
	if upserts[1].ProfileId != anonID {
		t.Errorf("soft-delete ProfileId = %q, want %q", upserts[1].ProfileId, anonID)
	}
	if !upserts[1].IsDeleted {
		t.Error("soft-delete IsDeleted = false, want true")
	}

	aliases := fetchAliases(t, ctx, js, 1)
	if len(aliases) != 1 {
		t.Fatalf("expected 1 alias, got %d", len(aliases))
	}
	if aliases[0].AliasId != anonID {
		t.Errorf("alias.AliasId = %q, want %q", aliases[0].AliasId, anonID)
	}
	if aliases[0].ProfileId != targetID {
		t.Errorf("alias.ProfileId = %q, want %q", aliases[0].ProfileId, targetID)
	}
	if aliases[0].ExternalId != "carol@test.com" {
		t.Errorf("alias.ExternalId = %q, want %q", aliases[0].ExternalId, "carol@test.com")
	}
}

func TestHandleIdentify_MergeOverlappingKeys(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	pg := testutil.SetupPostgres(t)
	projectID := seedProject(t, ctx, pg)
	natsClient, _ := setupNATSClient(t, ctx)

	write := dbwrite.New(pg.PgW)

	// Create anonymous profile with overlapping key "plan".
	anonID := xid.New().String()
	if _, err := write.RegisterProfile(ctx, dbwrite.RegisterProfileParams{
		ID:         anonID,
		ProjectID:  projectID,
		Properties: map[string]any{"plan": "free", "source": "web"},
	}); err != nil {
		t.Fatalf("register anon profile: %v", err)
	}

	w := profiles.NewWorker(pg.PgW)

	// Identify with traits that overlap on "plan" — target should win.
	data := makeIdentifyData(t, projectID, "overlap@test.com", anonID, "", map[string]any{"plan": "pro"})
	if err := handleIdentify(ctx, w, natsClient, data); err != nil {
		t.Fatalf("handleIdentify: %v", err)
	}

	var props map[string]any
	err := pg.PgW.QueryRow(ctx,
		`SELECT properties FROM profiles WHERE project_id = $1 AND external_id = $2`,
		projectID, "overlap@test.com",
	).Scan(&props)
	if err != nil {
		t.Fatalf("query profile: %v", err)
	}
	if props["plan"] != "pro" {
		t.Errorf("plan = %v, want %q (target traits should win)", props["plan"], "pro")
	}
	if props["source"] != "web" {
		t.Errorf("source = %v, want %q (should be preserved from anonymous)", props["source"], "web")
	}
}

func TestHandleIdentify_SelfMergeGuard(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	pg := testutil.SetupPostgres(t)
	projectID := seedProject(t, ctx, pg)
	natsClient, js := setupNATSClient(t, ctx)

	w := profiles.NewWorker(pg.PgW)

	// Create the identified profile.
	data1 := makeIdentifyData(t, projectID, "dave@test.com", "", "", nil)
	if err := handleIdentify(ctx, w, natsClient, data1); err != nil {
		t.Fatalf("first identify: %v", err)
	}

	// Get the created profile ID and verify nil traits produced empty properties.
	var profileID string
	var props map[string]any
	err := pg.PgW.QueryRow(ctx,
		`SELECT id, properties FROM profiles WHERE project_id = $1 AND external_id = $2`,
		projectID, "dave@test.com",
	).Scan(&profileID, &props)
	if err != nil {
		t.Fatalf("query profile: %v", err)
	}
	if len(props) != 0 {
		t.Errorf("properties = %v, want empty map for nil traits", props)
	}

	// Create a consumer that only sees messages published AFTER this point.
	consName := "test-selfmerge-" + xid.New().String()
	cons, err := js.CreateOrUpdateConsumer(ctx, "profiles", jetstream.ConsumerConfig{
		FilterSubject: natsworker.ProfileUpsertSubject,
		Name:          consName,
		DeliverPolicy: jetstream.DeliverNewPolicy,
	})
	if err != nil {
		t.Fatalf("create consumer: %v", err)
	}

	// Identify with anonymous_id == profile's own ID → should not self-delete.
	data2 := makeIdentifyData(t, projectID, "dave@test.com", profileID, "", nil)
	if err := handleIdentify(ctx, w, natsClient, data2); err != nil {
		t.Fatalf("self-merge identify: %v", err)
	}

	// Should publish only 1 upsert (no merge, no soft-delete, no alias).
	batch, err := cons.Fetch(2, jetstream.FetchMaxWait(2_000_000_000))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	var upsertCount int
	for range batch.Messages() {
		upsertCount++
	}
	if upsertCount != 1 {
		t.Fatalf("expected 1 upsert (no merge), got %d", upsertCount)
	}

	// Profile should still exist.
	var count int
	err = pg.PgW.QueryRow(ctx,
		`SELECT count(*) FROM profiles WHERE id = $1 AND project_id = $2`,
		profileID, projectID,
	).Scan(&count)
	if err != nil {
		t.Fatalf("count profile: %v", err)
	}
	if count != 1 {
		t.Errorf("profile count = %d, want 1 (should not self-delete)", count)
	}
}

func TestHandleIdentify_RetryAfterMerge(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	pg := testutil.SetupPostgres(t)
	projectID := seedProject(t, ctx, pg)
	natsClient, js := setupNATSClient(t, ctx)

	w := profiles.NewWorker(pg.PgW)

	// Create an identified profile directly.
	write := dbwrite.New(pg.PgW)
	targetID := xid.New().String()
	if _, err := write.UpsertProfileByExternalID(ctx, dbwrite.UpsertProfileByExternalIDParams{
		ID:         targetID,
		ProjectID:  projectID,
		ExternalID: postgres.NewText("eve@test.com"),
		Properties: map[string]any{"plan": "free"},
	}); err != nil {
		t.Fatalf("upsert target: %v", err)
	}

	// Identify with an anonymous_id that does NOT exist — simulates retry
	// where the anonymous profile was already merged and deleted.
	ghostAnonID := xid.New().String()
	data := makeIdentifyData(t, projectID, "eve@test.com", ghostAnonID, "", map[string]any{"plan": "pro"})
	if err := handleIdentify(ctx, w, natsClient, data); err != nil {
		t.Fatalf("retry identify: %v", err)
	}

	// Should succeed via ErrNoRows fallback — produces 2 upserts + 1 alias.
	upserts := fetchUpserts(t, ctx, js, 2)
	if len(upserts) != 2 {
		t.Fatalf("expected 2 upserts, got %d", len(upserts))
	}
	if upserts[0].ProfileId != targetID {
		t.Errorf("target upsert ProfileId = %q, want %q", upserts[0].ProfileId, targetID)
	}
	if upserts[1].ProfileId != ghostAnonID {
		t.Errorf("soft-delete ProfileId = %q, want %q", upserts[1].ProfileId, ghostAnonID)
	}
	if !upserts[1].IsDeleted {
		t.Error("soft-delete IsDeleted = false, want true")
	}
}

func TestHandleIdentify_UnmarshalError(t *testing.T) {
	w := profiles.NewWorker(nil)

	err := handleIdentify(context.Background(), w, nil, []byte("garbage"))
	if err == nil {
		t.Fatal("expected error for bad protobuf data")
	}

	if _, ok := errors.AsType[*natsworker.PermanentError](err); !ok {
		t.Errorf("expected PermanentError, got %T: %v", err, err)
	}
}

func TestHandleIdentify_MissingRequiredFields(t *testing.T) {
	w := profiles.NewWorker(nil)

	tests := []struct {
		name       string
		projectID  string
		externalID string
	}{
		{"empty projectId", "", "user@test.com"},
		{"empty externalId", "proj-123", ""},
		{"both empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := makeIdentifyData(t, tt.projectID, tt.externalID, "", "", nil)
			err := handleIdentify(context.Background(), w, nil, data)
			if err == nil {
				t.Fatal("expected error for missing required fields")
			}
			if _, ok := errors.AsType[*natsworker.PermanentError](err); !ok {
				t.Errorf("expected PermanentError, got %T: %v", err, err)
			}
		})
	}
}

func TestHandleIdentify_CrossProjectIsolation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	pg := testutil.SetupPostgres(t)
	projectA := seedProject(t, ctx, pg)
	projectB := seedProject(t, ctx, pg)
	natsClient, _ := setupNATSClient(t, ctx)

	write := dbwrite.New(pg.PgW)

	// Create an anonymous profile in project A.
	anonID := xid.New().String()
	if _, err := write.RegisterProfile(ctx, dbwrite.RegisterProfileParams{
		ID:         anonID,
		ProjectID:  projectA,
		Properties: map[string]any{"source": "web"},
	}); err != nil {
		t.Fatalf("register anon profile in project A: %v", err)
	}

	w := profiles.NewWorker(pg.PgW)

	// Identify in project B with anonymous_id from project A — should NOT merge.
	data := makeIdentifyData(t, projectB, "cross@test.com", anonID, "", map[string]any{"plan": "pro"})
	if err := handleIdentify(ctx, w, natsClient, data); err != nil {
		t.Fatalf("handleIdentify: %v", err)
	}

	// Anonymous profile in project A should still exist.
	var count int
	err := pg.PgW.QueryRow(ctx,
		`SELECT count(*) FROM profiles WHERE id = $1 AND project_id = $2`,
		anonID, projectA,
	).Scan(&count)
	if err != nil {
		t.Fatalf("count anon: %v", err)
	}
	if count != 1 {
		t.Errorf("anonymous profile in project A was deleted, want preserved (cross-project isolation)")
	}

	// Identified profile in project B should exist without anonymous properties.
	var props map[string]any
	err = pg.PgW.QueryRow(ctx,
		`SELECT properties FROM profiles WHERE project_id = $1 AND external_id = $2`,
		projectB, "cross@test.com",
	).Scan(&props)
	if err != nil {
		t.Fatalf("query profile in project B: %v", err)
	}
	if props["source"] == "web" {
		t.Error("project B profile got project A's anonymous properties — cross-project isolation broken")
	}
	if props["plan"] != "pro" {
		t.Errorf("plan = %v, want %q", props["plan"], "pro")
	}
}

func TestHandleIdentify_LinkDevice(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	pg := testutil.SetupPostgres(t)
	projectID := seedProject(t, ctx, pg)
	natsClient, _ := setupNATSClient(t, ctx)

	write := dbwrite.New(pg.PgW)

	// Register an anonymous device (NULL profile_id).
	deviceID := xid.New().String()
	if _, err := write.SaveProfileDevice(ctx, dbwrite.SaveProfileDeviceParams{
		ID:         deviceID,
		Platform:   "ios",
		ProfileID:  pgtype.Text{},
		ProjectID:  projectID,
		Properties: map[string]any{},
		Status:     "active",
		Token:      "token-link-test",
	}); err != nil {
		t.Fatalf("save anonymous device: %v", err)
	}

	w := profiles.NewWorker(pg.PgW)

	// Identify with device_id set — should link device to the identified profile.
	data := makeIdentifyData(t, projectID, "link@test.com", "", deviceID, nil)
	if err := handleIdentify(ctx, w, natsClient, data); err != nil {
		t.Fatalf("handleIdentify: %v", err)
	}

	// Look up the profile.
	var profileID string
	if err := pg.PgW.QueryRow(ctx,
		`SELECT id FROM profiles WHERE project_id = $1 AND external_id = $2`,
		projectID, "link@test.com",
	).Scan(&profileID); err != nil {
		t.Fatalf("query profile: %v", err)
	}

	// Verify device is now linked to the profile.
	var linkedProfileID pgtype.Text
	if err := pg.PgW.QueryRow(ctx,
		`SELECT profile_id FROM profile_devices WHERE id = $1 AND project_id = $2`,
		deviceID, projectID,
	).Scan(&linkedProfileID); err != nil {
		t.Fatalf("query device: %v", err)
	}
	if !linkedProfileID.Valid {
		t.Fatal("device profile_id is NULL, want linked profile")
	}
	if linkedProfileID.String != profileID {
		t.Errorf("device profile_id = %q, want %q", linkedProfileID.String, profileID)
	}
}

func TestHandleIdentify_DeviceAccountSwitch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	pg := testutil.SetupPostgres(t)
	projectID := seedProject(t, ctx, pg)
	natsClient, _ := setupNATSClient(t, ctx)

	write := dbwrite.New(pg.PgW)

	// Create profile A.
	profileAID := xid.New().String()
	if _, err := write.UpsertProfileByExternalID(ctx, dbwrite.UpsertProfileByExternalIDParams{
		ID:         profileAID,
		ProjectID:  projectID,
		ExternalID: postgres.NewText("profile-a@test.com"),
		Properties: map[string]any{},
	}); err != nil {
		t.Fatalf("upsert profile A: %v", err)
	}

	// Register device linked to profile A.
	deviceID := xid.New().String()
	if _, err := write.SaveProfileDevice(ctx, dbwrite.SaveProfileDeviceParams{
		ID:         deviceID,
		Platform:   "android",
		ProfileID:  postgres.NewText(profileAID),
		ProjectID:  projectID,
		Properties: map[string]any{},
		Status:     "active",
		Token:      "token-switch-test",
	}); err != nil {
		t.Fatalf("save device linked to profile A: %v", err)
	}

	w := profiles.NewWorker(pg.PgW)

	// Identify as profile B with the same device_id — account switch.
	data := makeIdentifyData(t, projectID, "profile-b@test.com", "", deviceID, nil)
	if err := handleIdentify(ctx, w, natsClient, data); err != nil {
		t.Fatalf("handleIdentify: %v", err)
	}

	// Look up profile B's ID.
	var profileBID string
	if err := pg.PgW.QueryRow(ctx,
		`SELECT id FROM profiles WHERE project_id = $1 AND external_id = $2`,
		projectID, "profile-b@test.com",
	).Scan(&profileBID); err != nil {
		t.Fatalf("query profile B: %v", err)
	}

	// Verify device moved from profile A to profile B.
	var linkedProfileID pgtype.Text
	if err := pg.PgW.QueryRow(ctx,
		`SELECT profile_id FROM profile_devices WHERE id = $1 AND project_id = $2`,
		deviceID, projectID,
	).Scan(&linkedProfileID); err != nil {
		t.Fatalf("query device: %v", err)
	}
	if !linkedProfileID.Valid {
		t.Fatal("device profile_id is NULL after account switch, want profile B")
	}
	if linkedProfileID.String == profileAID {
		t.Error("device still linked to profile A after account switch, want profile B")
	}
	if linkedProfileID.String != profileBID {
		t.Errorf("device profile_id = %q, want %q (profile B)", linkedProfileID.String, profileBID)
	}
}

func TestSoftDeleteDeactivatesDevices(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	pg := testutil.SetupPostgres(t)
	projectID := seedProject(t, ctx, pg)

	write := dbwrite.New(pg.PgW)

	// Create profile.
	profileID := xid.New().String()
	if _, err := write.UpsertProfileByExternalID(ctx, dbwrite.UpsertProfileByExternalIDParams{
		ID:         profileID,
		ProjectID:  projectID,
		ExternalID: postgres.NewText("delete-me@test.com"),
		Properties: map[string]any{},
	}); err != nil {
		t.Fatalf("upsert profile: %v", err)
	}

	// Create active device linked to profile.
	deviceID := xid.New().String()
	if _, err := write.SaveProfileDevice(ctx, dbwrite.SaveProfileDeviceParams{
		ID:         deviceID,
		Platform:   "ios",
		ProfileID:  postgres.NewText(profileID),
		ProjectID:  projectID,
		Properties: map[string]any{},
		Status:     "active",
		Token:      "token-deactivate-test",
	}); err != nil {
		t.Fatalf("save device: %v", err)
	}

	// Run soft-delete + deactivate in a transaction.
	tx, err := pg.PgW.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	qtx := write.WithTx(tx)

	if _, err := qtx.SoftDeleteProfileByIDAndProjectID(ctx, dbwrite.SoftDeleteProfileByIDAndProjectIDParams{
		ID:        profileID,
		ProjectID: projectID,
	}); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("soft delete profile: %v", err)
	}
	if _, err := qtx.DeactivateDevicesByProfileID(ctx, dbwrite.DeactivateDevicesByProfileIDParams{
		ProfileID: postgres.NewText(profileID),
		ProjectID: projectID,
	}); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("deactivate devices: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit tx: %v", err)
	}

	// Verify profile has deletion_time IS NOT NULL.
	var deletedAt *string
	if err := pg.PgW.QueryRow(ctx,
		`SELECT deletion_time::text FROM profiles WHERE id = $1 AND project_id = $2`,
		profileID, projectID,
	).Scan(&deletedAt); err != nil {
		t.Fatalf("query profile deletion_time: %v", err)
	}
	if deletedAt == nil {
		t.Error("profile deletion_time is NULL, want soft-deleted")
	}

	// Verify device has status = 'inactive'.
	var status string
	if err := pg.PgW.QueryRow(ctx,
		`SELECT status FROM profile_devices WHERE id = $1 AND project_id = $2`,
		deviceID, projectID,
	).Scan(&status); err != nil {
		t.Fatalf("query device status: %v", err)
	}
	if status != "inactive" {
		t.Errorf("device status = %q, want %q", status, "inactive")
	}
}

func TestSoftDeleteIdempotency(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	pg := testutil.SetupPostgres(t)
	projectID := seedProject(t, ctx, pg)

	write := dbwrite.New(pg.PgW)

	// Create profile.
	profileID := xid.New().String()
	if _, err := write.UpsertProfileByExternalID(ctx, dbwrite.UpsertProfileByExternalIDParams{
		ID:         profileID,
		ProjectID:  projectID,
		ExternalID: postgres.NewText("idem@test.com"),
		Properties: map[string]any{},
	}); err != nil {
		t.Fatalf("upsert profile: %v", err)
	}

	params := dbwrite.SoftDeleteProfileByIDAndProjectIDParams{
		ID:        profileID,
		ProjectID: projectID,
	}

	// First soft-delete — should affect 1 row.
	rows1, err := write.SoftDeleteProfileByIDAndProjectID(ctx, params)
	if err != nil {
		t.Fatalf("first soft delete: %v", err)
	}
	if rows1 != 1 {
		t.Errorf("first soft delete rows = %d, want 1", rows1)
	}

	// Second soft-delete — should affect 0 rows (already deleted).
	rows2, err := write.SoftDeleteProfileByIDAndProjectID(ctx, params)
	if err != nil {
		t.Fatalf("second soft delete: %v", err)
	}
	if rows2 != 0 {
		t.Errorf("second soft delete rows = %d, want 0 (idempotent)", rows2)
	}

	// Verify profile still exists in DB with deletion_time set.
	var deletedAt *string
	if err := pg.PgW.QueryRow(ctx,
		`SELECT deletion_time::text FROM profiles WHERE id = $1 AND project_id = $2`,
		profileID, projectID,
	).Scan(&deletedAt); err != nil {
		t.Fatalf("query profile deletion_time: %v", err)
	}
	if deletedAt == nil {
		t.Error("profile deletion_time is NULL after soft-delete, want set")
	}
}

func TestSoftDeletedProfileInvisibleToReads(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	pg := testutil.SetupPostgres(t)
	projectID := seedProject(t, ctx, pg)

	write := dbwrite.New(pg.PgW)
	read := dbread.New(pg.PgW)

	// Create an identified profile.
	profileID := xid.New().String()
	externalID := "visible@test.com"
	if _, err := write.UpsertProfileByExternalID(ctx, dbwrite.UpsertProfileByExternalIDParams{
		ID:         profileID,
		ProjectID:  projectID,
		ExternalID: postgres.NewText(externalID),
		Properties: map[string]any{},
	}); err != nil {
		t.Fatalf("upsert profile: %v", err)
	}

	// Verify GetProfileByIDAndProjectID finds it.
	if _, err := read.GetProfileByIDAndProjectID(ctx, dbread.GetProfileByIDAndProjectIDParams{
		ID:        profileID,
		ProjectID: projectID,
	}); err != nil {
		t.Fatalf("GetProfileByIDAndProjectID before delete: %v", err)
	}

	// Soft-delete the profile.
	if _, err := write.SoftDeleteProfileByIDAndProjectID(ctx, dbwrite.SoftDeleteProfileByIDAndProjectIDParams{
		ID:        profileID,
		ProjectID: projectID,
	}); err != nil {
		t.Fatalf("soft delete profile: %v", err)
	}

	// Verify GetProfileByIDAndProjectID returns pgx.ErrNoRows.
	_, err := read.GetProfileByIDAndProjectID(ctx, dbread.GetProfileByIDAndProjectIDParams{
		ID:        profileID,
		ProjectID: projectID,
	})
	if err == nil {
		t.Error("GetProfileByIDAndProjectID returned nil error after soft-delete, want pgx.ErrNoRows")
	} else if !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("GetProfileByIDAndProjectID error = %v, want pgx.ErrNoRows", err)
	}

	// Verify GetProfileByProjectAndExternalID returns pgx.ErrNoRows.
	_, err = read.GetProfileByProjectAndExternalID(ctx, dbread.GetProfileByProjectAndExternalIDParams{
		ProjectID:  projectID,
		ExternalID: externalID,
	})
	if err == nil {
		t.Error("GetProfileByProjectAndExternalID returned nil error after soft-delete, want pgx.ErrNoRows")
	} else if !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("GetProfileByProjectAndExternalID error = %v, want pgx.ErrNoRows", err)
	}
}

func TestReIdentifyAfterSoftDelete(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	pg := testutil.SetupPostgres(t)
	projectID := seedProject(t, ctx, pg)
	natsClient, _ := setupNATSClient(t, ctx)

	w := profiles.NewWorker(pg.PgW)
	read := dbread.New(pg.PgW)
	write := dbwrite.New(pg.PgW)

	// Step 1: Identify — creates profile.
	data := makeIdentifyData(t, projectID, "reident@test.com", "", "", nil)
	if err := handleIdentify(ctx, w, natsClient, data); err != nil {
		t.Fatalf("first identify: %v", err)
	}

	var firstID string
	if err := pg.PgW.QueryRow(ctx,
		`SELECT id FROM profiles WHERE project_id = $1 AND external_id = $2 AND deletion_time IS NULL`,
		projectID, "reident@test.com",
	).Scan(&firstID); err != nil {
		t.Fatalf("query first profile: %v", err)
	}

	// Step 2: Soft-delete the profile.
	if _, err := write.SoftDeleteProfileByIDAndProjectID(ctx, dbwrite.SoftDeleteProfileByIDAndProjectIDParams{
		ID:        firstID,
		ProjectID: projectID,
	}); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	// Step 3: Re-identify with the same external_id.
	data2 := makeIdentifyData(t, projectID, "reident@test.com", "", "", map[string]any{"plan": "new"})
	if err := handleIdentify(ctx, w, natsClient, data2); err != nil {
		t.Fatalf("re-identify: %v", err)
	}

	// Verify: new profile created with different ID.
	newProfile, err := read.GetProfileByProjectAndExternalID(ctx, dbread.GetProfileByProjectAndExternalIDParams{
		ProjectID:  projectID,
		ExternalID: "reident@test.com",
	})
	if err != nil {
		t.Fatalf("query new profile: %v", err)
	}
	if newProfile.ID == firstID {
		t.Errorf("re-identify reused soft-deleted profile ID %q, want new ID", firstID)
	}
	if newProfile.Properties["plan"] != "new" {
		t.Errorf("properties.plan = %v, want %q", newProfile.Properties["plan"], "new")
	}

	// Verify: old soft-deleted row still exists.
	var deletedAt *string
	if err := pg.PgW.QueryRow(ctx,
		`SELECT deletion_time::text FROM profiles WHERE id = $1 AND project_id = $2`,
		firstID, projectID,
	).Scan(&deletedAt); err != nil {
		t.Fatalf("query old profile: %v", err)
	}
	if deletedAt == nil {
		t.Error("old profile deletion_time is NULL, want soft-deleted")
	}
}

func TestMergeWithSoftDeletedAnonymousProfile(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	pg := testutil.SetupPostgres(t)
	projectID := seedProject(t, ctx, pg)
	natsClient, js := setupNATSClient(t, ctx)

	write := dbwrite.New(pg.PgW)

	// Create an anonymous profile then soft-delete it.
	anonID := xid.New().String()
	if _, err := write.RegisterProfile(ctx, dbwrite.RegisterProfileParams{
		ID:         anonID,
		ProjectID:  projectID,
		Properties: map[string]any{"source": "web"},
	}); err != nil {
		t.Fatalf("register anon profile: %v", err)
	}
	if _, err := write.SoftDeleteProfileByIDAndProjectID(ctx, dbwrite.SoftDeleteProfileByIDAndProjectIDParams{
		ID:        anonID,
		ProjectID: projectID,
	}); err != nil {
		t.Fatalf("soft delete anon: %v", err)
	}

	w := profiles.NewWorker(pg.PgW)

	// Identify with the soft-deleted anonymous_id — merge should gracefully skip.
	data := makeIdentifyData(t, projectID, "merge-deleted@test.com", anonID, "", map[string]any{"plan": "pro"})
	if err := handleIdentify(ctx, w, natsClient, data); err != nil {
		t.Fatalf("handleIdentify: %v", err)
	}

	// Verify: identified profile exists.
	var profileID string
	var props map[string]any
	if err := pg.PgW.QueryRow(ctx,
		`SELECT id, properties FROM profiles WHERE project_id = $1 AND external_id = $2 AND deletion_time IS NULL`,
		projectID, "merge-deleted@test.com",
	).Scan(&profileID, &props); err != nil {
		t.Fatalf("query profile: %v", err)
	}
	if props["plan"] != "pro" {
		t.Errorf("plan = %v, want %q", props["plan"], "pro")
	}
	// Anonymous properties should NOT be merged (source was soft-deleted).
	if props["source"] == "web" {
		t.Error("soft-deleted anonymous properties were merged, want skipped")
	}

	// Verify: NATS publishes still happened (target upsert + anon soft-delete).
	upserts := fetchUpserts(t, ctx, js, 2)
	if len(upserts) != 2 {
		t.Fatalf("expected 2 upserts, got %d", len(upserts))
	}
	if upserts[0].ProfileId != profileID {
		t.Errorf("target upsert ProfileId = %q, want %q", upserts[0].ProfileId, profileID)
	}
	if upserts[0].IsDeleted {
		t.Error("target upsert IsDeleted = true, want false")
	}
}

func TestSoftDeletedProfileInvisibleToList(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	pg := testutil.SetupPostgres(t)
	projectID := seedProject(t, ctx, pg)

	write := dbwrite.New(pg.PgW)
	read := dbread.New(pg.PgW)

	// Create two profiles.
	keepID := xid.New().String()
	if _, err := write.UpsertProfileByExternalID(ctx, dbwrite.UpsertProfileByExternalIDParams{
		ID:         keepID,
		ProjectID:  projectID,
		ExternalID: postgres.NewText("keep@test.com"),
		Properties: map[string]any{},
	}); err != nil {
		t.Fatalf("upsert keep profile: %v", err)
	}

	deleteID := xid.New().String()
	if _, err := write.UpsertProfileByExternalID(ctx, dbwrite.UpsertProfileByExternalIDParams{
		ID:         deleteID,
		ProjectID:  projectID,
		ExternalID: postgres.NewText("delete@test.com"),
		Properties: map[string]any{},
	}); err != nil {
		t.Fatalf("upsert delete profile: %v", err)
	}

	// Soft-delete one.
	if _, err := write.SoftDeleteProfileByIDAndProjectID(ctx, dbwrite.SoftDeleteProfileByIDAndProjectIDParams{
		ID:        deleteID,
		ProjectID: projectID,
	}); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	// List should return only the active profile.
	listed, err := read.GetProfilesByProjectID(ctx, dbread.GetProfilesByProjectIDParams{
		ProjectID: projectID,
		HasCursor: false,
		PageSize:  100,
	})
	if err != nil {
		t.Fatalf("list profiles: %v", err)
	}

	if len(listed) != 1 {
		t.Fatalf("listed %d profiles, want 1", len(listed))
	}
	if listed[0].ID != keepID {
		t.Errorf("listed profile ID = %q, want %q (active profile)", listed[0].ID, keepID)
	}
}

func TestBuildUpsertData_ValidationRejectsMissingFields(t *testing.T) {
	tests := []struct {
		name      string
		profileID string
		projectID string
	}{
		{name: "empty profile_id", profileID: "", projectID: "proj1"},
		{name: "empty project_id", profileID: "p1", projectID: ""},
		{name: "both empty", profileID: "", projectID: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildUpsertData(context.Background(), tc.profileID, tc.projectID, "ext1", nil, false, time.Now(), time.Now())
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !natsworker.IsPermanentError(err) {
				t.Errorf("expected PermanentError, got %T: %v", err, err)
			}
		})
	}
}

func TestBuildUpsertData_ValidMessage(t *testing.T) {
	data, err := buildUpsertData(context.Background(), "p1", "proj1", "ext1", map[string]any{"key": "val"}, false, time.Now(), time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("expected non-empty data")
	}
}

func TestBuildAliasData_ValidationRejectsMissingFields(t *testing.T) {
	tests := []struct {
		name        string
		anonymousID string
		targetID    string
		externalID  string
		projectID   string
	}{
		{name: "empty alias_id", anonymousID: "", targetID: "t1", externalID: "e1", projectID: "proj1"},
		{name: "empty profile_id", anonymousID: "a1", targetID: "", externalID: "e1", projectID: "proj1"},
		{name: "empty external_id", anonymousID: "a1", targetID: "t1", externalID: "", projectID: "proj1"},
		{name: "empty project_id", anonymousID: "a1", targetID: "t1", externalID: "e1", projectID: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildAliasData(context.Background(), tc.anonymousID, tc.targetID, tc.externalID, tc.projectID)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !natsworker.IsPermanentError(err) {
				t.Errorf("expected PermanentError, got %T: %v", err, err)
			}
		})
	}
}

func TestBuildAliasData_ValidMessage(t *testing.T) {
	data, err := buildAliasData(context.Background(), "anon1", "target1", "ext1", "proj1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("expected non-empty data")
	}
}
