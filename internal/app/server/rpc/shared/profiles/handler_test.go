package profiles

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/authn"
	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pug-sh/pug/internal/app/server/rpc"
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
			t.Fatal("expected panic for nil nats, got none")
		}
	}()
	NewServer(nil, nil, nil)
}

func TestNewServer_NonNilNATS(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("unexpected panic: %v", r)
		}
	}()
	// Provide a non-nil NATSClient; pgRO/pgW can be nil since we won't call DB methods.
	NewServer(nil, nil, &natsdeps.NATSClient{})
}

func TestDelete_Unauthenticated(t *testing.T) {
	s := NewServer(nil, nil, &natsdeps.NATSClient{})
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
	s := NewServer(nil, nil, &natsdeps.NATSClient{})
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

	s := NewServer(pg.PgRO, pg.PgW, natsClient)

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

	s := NewServer(pg.PgRO, pg.PgW, natsClient)

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

	s := NewServer(pg.PgRO, pg.PgW, natsClient)

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
	pg := testutil.SetupPostgres(t)
	projectID := seedProject(t, ctx, pg)
	write := dbwrite.New(pg.PgW)

	seedProfiles(t, ctx, write, projectID, pageSize)

	client := newProfilesTestClient(t, NewServer(pg.PgRO, pg.PgW, &natsdeps.NATSClient{}), projectID)
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
	pg := testutil.SetupPostgres(t)
	projectID := seedProject(t, ctx, pg)
	write := dbwrite.New(pg.PgW)

	seedProfiles(t, ctx, write, projectID, pageSize+1)

	client := newProfilesTestClient(t, NewServer(pg.PgRO, pg.PgW, &natsdeps.NATSClient{}), projectID)
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

func TestList_RejectsNonProfileFilterSources(t *testing.T) {
	s := NewServer(nil, nil, &natsdeps.NATSClient{})
	err := s.List(authCtx("proj_1"), connect.NewRequest(&profilesv1.ListRequest{
		FilterGroups: []*profilesv1.FilterGroup{
			{
				Filters: []*commonv1.PropertyFilter{
					{
						Property: proto.String("plan"),
						Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS.Enum(),
						Value:    proto.String("pro"),
						Source:   commonv1.PropertySource_PROPERTY_SOURCE_AUTO.Enum(),
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
	pg := testutil.SetupPostgres(t)
	projectID := seedProject(t, ctx, pg)
	write := dbwrite.New(pg.PgW)

	seedProfileWithProperties(t, ctx, write, projectID, "alice@example.com", map[string]any{
		"plan":       "pro",
		"age":        34,
		"subscribed": true,
	})
	seedProfileWithProperties(t, ctx, write, projectID, "bob@example.com", map[string]any{
		"plan":       "free",
		"age":        21,
		"subscribed": true,
	})
	seedProfileWithProperties(t, ctx, write, projectID, "carol@example.com", map[string]any{
		"plan":       "pro",
		"age":        18,
		"subscribed": false,
	})

	client := newProfilesTestClient(t, NewServer(pg.PgRO, pg.PgW, &natsdeps.NATSClient{}), projectID)
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
	pg := testutil.SetupPostgres(t)
	projectID := seedProject(t, ctx, pg)
	write := dbwrite.New(pg.PgW)

	for i := range pageSize + 1 {
		seedProfileWithProperties(t, ctx, write, projectID, fmt.Sprintf("pro-%03d@example.com", i), map[string]any{
			"segment": "pro",
		})
	}
	for i := range 5 {
		seedProfileWithProperties(t, ctx, write, projectID, fmt.Sprintf("free-%03d@example.com", i), map[string]any{
			"segment": "free",
		})
	}

	client := newProfilesTestClient(t, NewServer(pg.PgRO, pg.PgW, &natsdeps.NATSClient{}), projectID)
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

func seedProfiles(t *testing.T, ctx context.Context, write *dbwrite.Queries, projectID string, count int) {
	t.Helper()
	for i := range count {
		seedProfileWithProperties(t, ctx, write, projectID, fmt.Sprintf("user-%03d@example.com", i), map[string]any{"index": i})
	}
}

func seedProfileWithProperties(t *testing.T, ctx context.Context, write *dbwrite.Queries, projectID, externalID string, properties map[string]any) {
	t.Helper()
	if _, err := write.UpsertProfileByExternalID(ctx, dbwrite.UpsertProfileByExternalIDParams{
		ID:         xid.New().String(),
		ProjectID:  projectID,
		ExternalID: postgres.NewText(externalID),
		Properties: properties,
	}); err != nil {
		t.Fatalf("upsert profile %q: %v", externalID, err)
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
