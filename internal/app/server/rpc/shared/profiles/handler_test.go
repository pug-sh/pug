package profiles

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/authn"
	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pug-sh/pug/internal/app/server/rpc"
	coreprofiles "github.com/pug-sh/pug/internal/core/profiles"
	natsdeps "github.com/pug-sh/pug/internal/deps/nats"
	"github.com/pug-sh/pug/internal/deps/postgres"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	profilesv1 "github.com/pug-sh/pug/internal/gen/proto/shared/profiles/v1"
	profilesv1connect "github.com/pug-sh/pug/internal/gen/proto/shared/profiles/v1/profilesv1connect"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
	"github.com/rs/xid"
	"google.golang.org/protobuf/proto"
)

func TestNewServer_NilNATSPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for nil service, got none")
		}
	}()
	NewServer(nil)
}

func TestNewServer_NonNilService(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("unexpected panic: %v", r)
		}
	}()
	NewServer(coreprofiles.NewService(nil, nil, &natsdeps.NATSClient{}))
}

func TestDelete_Unauthenticated(t *testing.T) {
	s := NewServer(coreprofiles.NewService(nil, nil, &natsdeps.NATSClient{}))
	id := proto.String("p1")
	_, err := s.Delete(context.Background(), connect.NewRequest(&profilesv1.DeleteRequest{Id: id}))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if code := connect.CodeOf(err); code != connect.CodeUnauthenticated {
		t.Errorf("got code %v, want CodeUnauthenticated", code)
	}
}

func TestGet_Unauthenticated(t *testing.T) {
	s := NewServer(coreprofiles.NewService(nil, nil, &natsdeps.NATSClient{}))
	id := proto.String("p1")
	_, err := s.Get(context.Background(), connect.NewRequest(&profilesv1.GetRequest{Id: id}))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if code := connect.CodeOf(err); code != connect.CodeUnauthenticated {
		t.Errorf("got code %v, want CodeUnauthenticated", code)
	}
}

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

func authCtx(projectID string) context.Context {
	return authn.SetInfo(context.Background(), &rpc.Principal{
		Project: &dbread.Project{ID: projectID},
	})
}

func TestDelete_SoftDeleteAndDeactivateDevices(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	pg := testutil.SetupPostgres(t)
	tn := testutil.SetupNATS(t)
	t.Setenv("NATS_URL", tn.URL)

	natsClient, err := natsdeps.New(ctx)
	if err != nil {
		t.Fatalf("create nats client: %v", err)
	}
	defer natsClient.Close()

	projectID := seedProject(t, ctx, pg)
	write := dbwrite.New(pg.PgW)

	// Create a profile.
	profileID := xid.New().String()
	_, err = write.UpsertProfileByExternalID(ctx, dbwrite.UpsertProfileByExternalIDParams{
		ID:         profileID,
		ProjectID:  projectID,
		ExternalID: postgres.NewText("alice@test.com"),
		Properties: map[string]any{},
	})
	if err != nil {
		t.Fatalf("upsert profile: %v", err)
	}

	// Register two active devices linked to the profile.
	for _, devID := range []string{xid.New().String(), xid.New().String()} {
		if _, err := write.SaveProfileDevice(ctx, dbwrite.SaveProfileDeviceParams{
			ID:         devID,
			Platform:   "ios",
			ProfileID:  postgres.NewText(profileID),
			ProjectID:  projectID,
			Properties: map[string]any{},
			Status:     "active",
			Token:      "tok-" + devID,
		}); err != nil {
			t.Fatalf("save device: %v", err)
		}
	}

	s := NewServer(coreprofiles.NewService(pg.PgW, nil, natsClient))

	// Delete the profile via the handler.
	delID := proto.String(profileID)
	_, err = s.Delete(authCtx(projectID), connect.NewRequest(&profilesv1.DeleteRequest{Id: delID}))
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Verify profile is soft-deleted (has deletion_time set).
	var deletionTime pgtype.Timestamptz
	err = pg.PgW.QueryRow(ctx,
		`SELECT deletion_time FROM profiles WHERE id = $1 AND project_id = $2`,
		profileID, projectID,
	).Scan(&deletionTime)
	if err != nil {
		t.Fatalf("query profile: %v", err)
	}
	if !deletionTime.Valid {
		t.Error("deletion_time is NULL, want non-NULL after soft-delete")
	}

	// Verify all devices are deactivated.
	var activeCount int
	err = pg.PgW.QueryRow(ctx,
		`SELECT count(*) FROM profile_devices WHERE profile_id = $1 AND project_id = $2 AND status = 'active'`,
		profileID, projectID,
	).Scan(&activeCount)
	if err != nil {
		t.Fatalf("count active devices: %v", err)
	}
	if activeCount != 0 {
		t.Errorf("active devices = %d, want 0 after delete", activeCount)
	}
}

func TestDelete_AlreadyDeleted(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	pg := testutil.SetupPostgres(t)
	tn := testutil.SetupNATS(t)
	t.Setenv("NATS_URL", tn.URL)

	natsClient, err := natsdeps.New(ctx)
	if err != nil {
		t.Fatalf("create nats client: %v", err)
	}
	defer natsClient.Close()

	projectID := seedProject(t, ctx, pg)
	write := dbwrite.New(pg.PgW)

	profileID := xid.New().String()
	_, err = write.UpsertProfileByExternalID(ctx, dbwrite.UpsertProfileByExternalIDParams{
		ID:         profileID,
		ProjectID:  projectID,
		ExternalID: postgres.NewText("bob@test.com"),
		Properties: map[string]any{},
	})
	if err != nil {
		t.Fatalf("upsert profile: %v", err)
	}

	s := NewServer(coreprofiles.NewService(pg.PgW, nil, natsClient))

	// First delete succeeds.
	delID := proto.String(profileID)
	_, err = s.Delete(authCtx(projectID), connect.NewRequest(&profilesv1.DeleteRequest{Id: delID}))
	if err != nil {
		t.Fatalf("first Delete: %v", err)
	}

	// Second delete returns CodeNotFound.
	delID = proto.String(profileID)
	_, err = s.Delete(authCtx(projectID), connect.NewRequest(&profilesv1.DeleteRequest{Id: delID}))
	if err == nil {
		t.Fatal("expected error for already-deleted profile, got nil")
	}
	if code := connect.CodeOf(err); code != connect.CodeNotFound {
		t.Errorf("got code %v, want CodeNotFound", code)
	}
}

func TestDelete_NonExistent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	pg := testutil.SetupPostgres(t)
	tn := testutil.SetupNATS(t)
	t.Setenv("NATS_URL", tn.URL)

	natsClient, err := natsdeps.New(ctx)
	if err != nil {
		t.Fatalf("create nats client: %v", err)
	}
	defer natsClient.Close()

	projectID := seedProject(t, ctx, pg)

	s := NewServer(coreprofiles.NewService(pg.PgW, nil, natsClient))

	delID := proto.String("nonexistent-id")
	_, err = s.Delete(authCtx(projectID), connect.NewRequest(&profilesv1.DeleteRequest{Id: delID}))
	if err == nil {
		t.Fatal("expected error for non-existent profile, got nil")
	}
	if code := connect.CodeOf(err); code != connect.CodeNotFound {
		t.Errorf("got code %v, want CodeNotFound", code)
	}
}

func TestList_ExactPageSizeOmitsNextPageToken(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	ch := testutil.SetupClickHouse(t)
	projectID := xid.New().String()

	seedCHProfiles(t, ctx, ch, projectID, pageSize)

	client := newProfilesTestClient(t, NewServer(coreprofiles.NewService(nil, ch.Conn, &natsdeps.NATSClient{})), projectID)
	stream, err := client.List(ctx, connect.NewRequest(&profilesv1.ListRequest{}))
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	var responses []*profilesv1.ListResponse
	for stream.Receive() {
		responses = append(responses, stream.Msg())
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream err: %v", err)
	}

	if len(responses) != 1 {
		t.Fatalf("responses = %d, want 1", len(responses))
	}
	if got := len(responses[0].GetProfiles()); got != pageSize {
		t.Fatalf("profiles in first response = %d, want %d", got, pageSize)
	}
	if got := responses[0].GetNextPageToken(); got != "" {
		t.Fatalf("next_page_token = %q, want empty", got)
	}
}

func TestList_MoreThanPageSizeStreamsSecondPage(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	ch := testutil.SetupClickHouse(t)
	projectID := xid.New().String()

	seedCHProfiles(t, ctx, ch, projectID, pageSize+1)

	client := newProfilesTestClient(t, NewServer(coreprofiles.NewService(nil, ch.Conn, &natsdeps.NATSClient{})), projectID)
	stream, err := client.List(ctx, connect.NewRequest(&profilesv1.ListRequest{}))
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	var responses []*profilesv1.ListResponse
	for stream.Receive() {
		responses = append(responses, stream.Msg())
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream err: %v", err)
	}

	if len(responses) != 2 {
		t.Fatalf("responses = %d, want 2", len(responses))
	}
	if got := len(responses[0].GetProfiles()); got != pageSize {
		t.Fatalf("profiles in first response = %d, want %d", got, pageSize)
	}
	if got := responses[0].GetNextPageToken(); got == "" {
		t.Fatal("first response next_page_token is empty, want non-empty")
	}
	if got := len(responses[1].GetProfiles()); got != 1 {
		t.Fatalf("profiles in second response = %d, want 1", got)
	}
	if got := responses[1].GetNextPageToken(); got != "" {
		t.Fatalf("second response next_page_token = %q, want empty", got)
	}
}

func TestList_RejectsUnsupportedFilterSources(t *testing.T) {
	s := NewServer(coreprofiles.NewService(nil, nil, &natsdeps.NATSClient{}))
	err := s.List(authCtx("proj_1"), connect.NewRequest(&profilesv1.ListRequest{
		FilterGroups: []*profilesv1.FilterGroup{
			{
				Filters: []*commonv1.PropertyFilter{
					{
						Property: proto.String("plan"),
						Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS.Enum(),
						Value:    proto.String("pro"),
						Source:   commonv1.PropertySource_PROPERTY_SOURCE_CUSTOM.Enum(),
					},
				},
			},
		},
	}), nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if code := connect.CodeOf(err); code != connect.CodeInvalidArgument {
		t.Fatalf("got code %v, want CodeInvalidArgument", code)
	}
}

func TestList_FiltersProfilesByProperties(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	ch := testutil.SetupClickHouse(t)
	projectID := xid.New().String()

	seedCHProfileWithProperties(t, ctx, ch, projectID, "alice@example.com", map[string]any{
		"plan":       "pro",
		"age":        34,
		"subscribed": true,
	})
	seedCHProfileWithProperties(t, ctx, ch, projectID, "bob@example.com", map[string]any{
		"plan":       "free",
		"age":        21,
		"subscribed": true,
	})
	seedCHProfileWithProperties(t, ctx, ch, projectID, "carol@example.com", map[string]any{
		"plan":       "pro",
		"age":        18,
		"subscribed": false,
	})

	client := newProfilesTestClient(t, NewServer(coreprofiles.NewService(nil, ch.Conn, &natsdeps.NATSClient{})), projectID)
	stream, err := client.List(ctx, connect.NewRequest(&profilesv1.ListRequest{
		FilterGroups: []*profilesv1.FilterGroup{
			{
				Filters: []*commonv1.PropertyFilter{
					{
						Property: proto.String("plan"),
						Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS.Enum(),
						Value:    proto.String("pro"),
					},
					{
						Property: proto.String("age"),
						Operator: commonv1.FilterOperator_FILTER_OPERATOR_GTE.Enum(),
						Value:    proto.String("30"),
					},
					{
						Property: proto.String("subscribed"),
						Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS.Enum(),
						Value:    proto.String("true"),
					},
				},
			},
		},
	}))
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	var got []string
	for stream.Receive() {
		for _, p := range stream.Msg().GetProfiles() {
			got = append(got, p.GetExternalId())
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream err: %v", err)
	}

	if len(got) != 1 || got[0] != "alice@example.com" {
		t.Fatalf("external_ids = %v, want [alice@example.com]", got)
	}
}

func TestList_FilteredPagination(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	ch := testutil.SetupClickHouse(t)
	projectID := xid.New().String()

	for i := range pageSize + 1 {
		seedCHProfileWithProperties(t, ctx, ch, projectID, fmt.Sprintf("pro-%03d@example.com", i), map[string]any{
			"segment": "pro",
		})
	}
	for i := range 5 {
		seedCHProfileWithProperties(t, ctx, ch, projectID, fmt.Sprintf("free-%03d@example.com", i), map[string]any{
			"segment": "free",
		})
	}

	client := newProfilesTestClient(t, NewServer(coreprofiles.NewService(nil, ch.Conn, &natsdeps.NATSClient{})), projectID)
	stream, err := client.List(ctx, connect.NewRequest(&profilesv1.ListRequest{
		FilterGroups: []*profilesv1.FilterGroup{
			{
				Filters: []*commonv1.PropertyFilter{
					{
						Property: proto.String("segment"),
						Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS.Enum(),
						Value:    proto.String("pro"),
					},
				},
			},
		},
	}))
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	var responses []*profilesv1.ListResponse
	for stream.Receive() {
		responses = append(responses, stream.Msg())
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream err: %v", err)
	}

	if len(responses) != 2 {
		t.Fatalf("responses = %d, want 2", len(responses))
	}
	if got := len(responses[0].GetProfiles()); got != pageSize {
		t.Fatalf("profiles in first response = %d, want %d", got, pageSize)
	}
	if got := responses[0].GetNextPageToken(); got == "" {
		t.Fatal("first response next_page_token is empty, want non-empty")
	}
	if got := len(responses[1].GetProfiles()); got != 1 {
		t.Fatalf("profiles in second response = %d, want 1", got)
	}
	for _, resp := range responses {
		for _, p := range resp.GetProfiles() {
			if p.GetProperties().GetFields()["segment"].GetStringValue() != "pro" {
				t.Fatalf("profile %q has segment %q, want pro", p.GetExternalId(), p.GetProperties().GetFields()["segment"].GetStringValue())
			}
		}
	}
}

func TestConvertActivitySummary(t *testing.T) {
	now := time.Date(2026, 5, 8, 10, 11, 12, 0, time.UTC)
	got := convertActivitySummary(&coreprofiles.ProfileActivitySummary{
		FirstSeen:      now.Add(-time.Hour),
		LastSeen:       now,
		TotalEvents:    42,
		Pageviews:      17,
		Sessions:       5,
		Browser:        "Chrome",
		BrowserVersion: "136",
		OS:             "macOS",
		OSVersion:      "15",
		Device:         "Desktop",
		Country:        "US",
		Region:         "California",
		City:           "San Francisco",
	})
	if got == nil {
		t.Fatal("convertActivitySummary() = nil, want non-nil")
	}
	if got.GetTotalEvents() != 42 {
		t.Fatalf("TotalEvents = %d, want 42", got.GetTotalEvents())
	}
	if got.GetPageviews() != 17 {
		t.Fatalf("Pageviews = %d, want 17", got.GetPageviews())
	}
	if got.GetSessions() != 5 {
		t.Fatalf("Sessions = %d, want 5", got.GetSessions())
	}
	if got.GetBrowser() != "Chrome" {
		t.Fatalf("Browser = %q, want Chrome", got.GetBrowser())
	}
	if got.GetCountry() != "US" {
		t.Fatalf("Country = %q, want US", got.GetCountry())
	}
	if got.GetFirstSeen().AsTime() != now.Add(-time.Hour) {
		t.Fatalf("FirstSeen = %v, want %v", got.GetFirstSeen().AsTime(), now.Add(-time.Hour))
	}
	if got.GetLastSeen().AsTime() != now {
		t.Fatalf("LastSeen = %v, want %v", got.GetLastSeen().AsTime(), now)
	}
}

// TestGet_ReturnsProfile pins the GetByID happy path through the
// scan + unwrap chain against a real CH JSON column. The List integration
// path covers a different SQL shape (create_time ordering); this test
// independently exercises the getSingle path (update_time DESC LIMIT 1).
func TestGet_ReturnsProfile(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	ch := testutil.SetupClickHouse(t)
	projectID := xid.New().String()
	profileID := xid.New().String()

	seedCHProfileWithID(t, ctx, ch, projectID, profileID, "alice@example.com", map[string]any{
		"plan": "pro",
		"age":  42,
	})

	client := newProfilesTestClient(t, NewServer(coreprofiles.NewService(nil, ch.Conn, &natsdeps.NATSClient{})), projectID)
	resp, err := client.Get(ctx, connect.NewRequest(&profilesv1.GetRequest{Id: proto.String(profileID)}))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	got := resp.Msg.GetProfile()
	if got.GetId() != profileID {
		t.Errorf("id = %q, want %q", got.GetId(), profileID)
	}
	if got.GetExternalId() != "alice@example.com" {
		t.Errorf("external_id = %q, want alice@example.com", got.GetExternalId())
	}
	props := got.GetProperties().AsMap()
	if props["plan"] != "pro" {
		t.Errorf("properties.plan = %v, want pro", props["plan"])
	}
	if props["age"] != float64(42) { // structpb converts numerics to float64
		t.Errorf("properties.age = %v, want 42", props["age"])
	}
}

// TestGetByExternalId_ReturnsProfile pins the GetByExternalId happy path.
// Same scan + unwrap chain as Get, different WHERE clause.
func TestGetByExternalId_ReturnsProfile(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	ch := testutil.SetupClickHouse(t)
	projectID := xid.New().String()

	seedCHProfileWithProperties(t, ctx, ch, projectID, "bob@example.com", map[string]any{
		"verified": true,
		"score":    99.5,
	})

	client := newProfilesTestClient(t, NewServer(coreprofiles.NewService(nil, ch.Conn, &natsdeps.NATSClient{})), projectID)
	resp, err := client.GetByExternalId(ctx, connect.NewRequest(&profilesv1.GetByExternalIdRequest{
		ExternalId: proto.String("bob@example.com"),
	}))
	if err != nil {
		t.Fatalf("GetByExternalId: %v", err)
	}

	got := resp.Msg.GetProfile()
	if got.GetExternalId() != "bob@example.com" {
		t.Errorf("external_id = %q, want bob@example.com", got.GetExternalId())
	}
	props := got.GetProperties().AsMap()
	if props["verified"] != true {
		t.Errorf("properties.verified = %v, want true", props["verified"])
	}
	if props["score"] != float64(99.5) {
		t.Errorf("properties.score = %v, want 99.5", props["score"])
	}
}

// TestList_BoolPropertyExcludedFromNumericFilter pins the load-bearing
// invariant on ProfilePropertyNumericExpr: a Bool-stored property must NOT
// match a numeric comparison. CH would otherwise coerce true → 1 with a
// direct CAST(JSON AS Float64), producing surprising filter results — the
// coalesce ladder explicitly omits .:Bool so this returns NULL (no match).
func TestList_BoolPropertyExcludedFromNumericFilter(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	ch := testutil.SetupClickHouse(t)
	projectID := xid.New().String()

	// verified = true (Bool), score = 42 (Int64). Numeric filter on the Bool
	// property must not match; the same filter on the Int64 property must.
	seedCHProfileWithProperties(t, ctx, ch, projectID, "alice@example.com", map[string]any{
		"verified": true,
		"score":    42,
	})

	client := newProfilesTestClient(t, NewServer(coreprofiles.NewService(nil, ch.Conn, &natsdeps.NATSClient{})), projectID)

	// Numeric filter on Bool-stored property: zero matches.
	verifiedStream, err := client.List(ctx, connect.NewRequest(&profilesv1.ListRequest{
		FilterGroups: []*profilesv1.FilterGroup{{
			Filters: []*commonv1.PropertyFilter{{
				Property: proto.String("verified"),
				Operator: commonv1.FilterOperator_FILTER_OPERATOR_GTE.Enum(),
				Value:    proto.String("0.5"),
			}},
		}},
	}))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var verifiedHits []string
	for verifiedStream.Receive() {
		for _, p := range verifiedStream.Msg().GetProfiles() {
			verifiedHits = append(verifiedHits, p.GetExternalId())
		}
	}
	if err := verifiedStream.Err(); err != nil {
		t.Fatalf("verified stream err: %v", err)
	}
	if len(verifiedHits) != 0 {
		t.Errorf("verified >= 0.5 matched %d profiles, want 0 (Bool excluded from numeric projection)", len(verifiedHits))
	}

	// Sanity check: same operator on the Int64 property matches.
	scoreStream, err := client.List(ctx, connect.NewRequest(&profilesv1.ListRequest{
		FilterGroups: []*profilesv1.FilterGroup{{
			Filters: []*commonv1.PropertyFilter{{
				Property: proto.String("score"),
				Operator: commonv1.FilterOperator_FILTER_OPERATOR_GTE.Enum(),
				Value:    proto.String("10"),
			}},
		}},
	}))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var scoreHits []string
	for scoreStream.Receive() {
		for _, p := range scoreStream.Msg().GetProfiles() {
			scoreHits = append(scoreHits, p.GetExternalId())
		}
	}
	if err := scoreStream.Err(); err != nil {
		t.Fatalf("score stream err: %v", err)
	}
	if len(scoreHits) != 1 || scoreHits[0] != "alice@example.com" {
		t.Errorf("score >= 10 hits = %v, want [alice@example.com] (sanity check that numeric filters do work on Int64)", scoreHits)
	}
}

func seedCHProfileWithID(t *testing.T, ctx context.Context, ch *testutil.TestClickHouse, projectID, profileID, externalID string, properties map[string]any) {
	t.Helper()

	rawProperties, err := json.Marshal(properties)
	if err != nil {
		t.Fatalf("marshal properties for %q: %v", externalID, err)
	}

	now := time.Now().UTC()
	if err := ch.Conn.Exec(ctx,
		`INSERT INTO profiles (id, project_id, external_id, properties, is_deleted, create_time, update_time) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		profileID,
		projectID,
		externalID,
		string(rawProperties),
		uint8(0),
		now,
		now,
	); err != nil {
		t.Fatalf("insert clickhouse profile %q: %v", externalID, err)
	}
}

func seedCHProfiles(t *testing.T, ctx context.Context, ch *testutil.TestClickHouse, projectID string, count int) {
	t.Helper()
	for i := range count {
		seedCHProfileWithProperties(t, ctx, ch, projectID, fmt.Sprintf("user-%03d@example.com", i), map[string]any{"index": i})
	}
}

func seedCHProfileWithProperties(t *testing.T, ctx context.Context, ch *testutil.TestClickHouse, projectID, externalID string, properties map[string]any) {
	t.Helper()

	rawProperties, err := json.Marshal(properties)
	if err != nil {
		t.Fatalf("marshal properties for %q: %v", externalID, err)
	}

	now := time.Now().UTC()
	if err := ch.Conn.Exec(ctx,
		`INSERT INTO profiles (id, project_id, external_id, properties, is_deleted, create_time, update_time) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		xid.New().String(),
		projectID,
		externalID,
		string(rawProperties),
		uint8(0),
		now,
		now,
	); err != nil {
		t.Fatalf("insert clickhouse profile %q: %v", externalID, err)
	}
}

func newProfilesTestClient(t *testing.T, svc *Server, projectID string) profilesv1connect.ProfilesServiceClient {
	t.Helper()

	path, handler := profilesv1connect.NewProfilesServiceHandler(svc)
	authMW := authn.NewMiddleware(func(ctx context.Context, req *http.Request) (any, error) {
		return &rpc.Principal{Project: &dbread.Project{ID: projectID}}, nil
	})

	mux := http.NewServeMux()
	mux.Handle(path, authMW.Wrap(handler))

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	return profilesv1connect.NewProfilesServiceClient(http.DefaultClient, ts.URL)
}
