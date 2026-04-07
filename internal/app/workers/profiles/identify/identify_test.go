package identify

import (
	"context"
	"errors"
	"testing"

	"github.com/fivebitsio/cotton/internal/app/workers/profiles"
	natsworker "github.com/fivebitsio/cotton/internal/deps/nats"
	sdkprofilesv1 "github.com/fivebitsio/cotton/internal/gen/proto/sdk/profiles/v1"
	workerprofilesv1 "github.com/fivebitsio/cotton/internal/gen/proto/workers/profiles/v1"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/fivebitsio/cotton/internal/testutil"
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

func makeIdentifyData(t *testing.T, projectID, externalID, anonymousID string, traits map[string]any) []byte {
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

	w := profiles.NewWorker(nil, pg.PgW)

	data := makeIdentifyData(t, projectID, "alice@example.com", "", map[string]any{"plan": "pro"})
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

	w := profiles.NewWorker(nil, pg.PgW)

	// First identify: creates profile.
	data1 := makeIdentifyData(t, projectID, "bob@test.com", "", map[string]any{"plan": "free"})
	if err := handleIdentify(ctx, w, natsClient, data1); err != nil {
		t.Fatalf("first identify: %v", err)
	}

	// Second identify: merges traits — new traits overwrite existing keys.
	data2 := makeIdentifyData(t, projectID, "bob@test.com", "", map[string]any{"plan": "pro", "role": "admin"})
	if err := handleIdentify(ctx, w, natsClient, data2); err != nil {
		t.Fatalf("second identify: %v", err)
	}

	var props map[string]any
	err := pg.PgW.QueryRow(ctx,
		`SELECT properties FROM profiles WHERE project_id = $1 AND external_id = $2`,
		projectID, "bob@test.com",
	).Scan(&props)
	if err != nil {
		t.Fatalf("query profile: %v", err)
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
		ProfileID:  anonID,
		ProjectID:  projectID,
		Properties: map[string]any{},
		Status:     "active",
		Token:      "test-token-abc",
	}); err != nil {
		t.Fatalf("save device: %v", err)
	}

	w := profiles.NewWorker(nil, pg.PgW)

	// Identify with anonymous_id → triggers merge.
	data := makeIdentifyData(t, projectID, "carol@test.com", anonID, map[string]any{"plan": "enterprise"})
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

	// Verify: anonymous profile is deleted.
	var count int
	err = pg.PgW.QueryRow(ctx,
		`SELECT count(*) FROM profiles WHERE id = $1 AND project_id = $2`,
		anonID, projectID,
	).Scan(&count)
	if err != nil {
		t.Fatalf("count anon: %v", err)
	}
	if count != 0 {
		t.Errorf("anonymous profile still exists, want deleted")
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

func TestHandleIdentify_SelfMergeGuard(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	pg := testutil.SetupPostgres(t)
	projectID := seedProject(t, ctx, pg)
	natsClient, js := setupNATSClient(t, ctx)

	w := profiles.NewWorker(nil, pg.PgW)

	// Create the identified profile.
	data1 := makeIdentifyData(t, projectID, "dave@test.com", "", nil)
	if err := handleIdentify(ctx, w, natsClient, data1); err != nil {
		t.Fatalf("first identify: %v", err)
	}

	// Get the created profile ID.
	var profileID string
	err := pg.PgW.QueryRow(ctx,
		`SELECT id FROM profiles WHERE project_id = $1 AND external_id = $2`,
		projectID, "dave@test.com",
	).Scan(&profileID)
	if err != nil {
		t.Fatalf("query profile: %v", err)
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
	data2 := makeIdentifyData(t, projectID, "dave@test.com", profileID, nil)
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

	w := profiles.NewWorker(nil, pg.PgW)

	// Create an identified profile directly.
	write := dbwrite.New(pg.PgW)
	targetID := xid.New().String()
	if _, err := write.UpsertProfileByExternalID(ctx, dbwrite.UpsertProfileByExternalIDParams{
		ID:         targetID,
		ProjectID:  projectID,
		ExternalID: pgtype.Text{String: "eve@test.com", Valid: true},
		Properties: map[string]any{"plan": "free"},
	}); err != nil {
		t.Fatalf("upsert target: %v", err)
	}

	// Identify with an anonymous_id that does NOT exist — simulates retry
	// where the anonymous profile was already merged and deleted.
	ghostAnonID := xid.New().String()
	data := makeIdentifyData(t, projectID, "eve@test.com", ghostAnonID, map[string]any{"plan": "pro"})
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
	w := profiles.NewWorker(nil, nil)

	err := handleIdentify(context.Background(), w, nil, []byte("garbage"))
	if err == nil {
		t.Fatal("expected error for bad protobuf data")
	}

	if _, ok := errors.AsType[*natsworker.PermanentError](err); !ok {
		t.Errorf("expected PermanentError, got %T: %v", err, err)
	}
}
