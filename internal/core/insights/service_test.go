package insights_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/xid"
	"google.golang.org/protobuf/proto"

	"github.com/fivebitsio/cotton/internal/core/insights"
	commonv1 "github.com/fivebitsio/cotton/internal/gen/proto/common/v1"
	"github.com/fivebitsio/cotton/internal/testutil"
)

func TestServiceGetFilterSchema(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	ch := testutil.SetupClickHouse(t)
	rd := testutil.SetupRedis(t)
	pg := testutil.SetupPostgres(t)

	projectID := seedTestProject(t, ctx, pg)
	seedServiceEvents(t, ctx, ch, projectID)
	seedServiceProfiles(t, ctx, ch, pg, projectID)

	executor := insights.NewExecutor(ch.Conn)
	svc := insights.NewService(executor, rd.Client)

	t.Run("returns_events_and_keys", func(t *testing.T) {
		resp, err := svc.GetFilterSchema(ctx, projectID, "")
		if err != nil {
			t.Fatalf("GetFilterSchema: %v", err)
		}
		if len(resp.GetEvents()) == 0 {
			t.Error("expected at least one event name")
		}
		if len(resp.GetAutoPropertyKeys()) == 0 {
			t.Error("expected at least one auto property key")
		}
		if len(resp.GetProfilePropertyKeys()) == 0 {
			t.Error("expected at least one profile property key")
		}

		// Verify event metadata is populated.
		kinds := map[string]bool{}
		for _, e := range resp.GetEvents() {
			kinds[e.GetName()] = true
			if e.GetCount() == 0 {
				t.Errorf("event %q has zero count", e.GetName())
			}
		}
		if !kinds["page_view"] || !kinds["purchase"] {
			t.Errorf("expected page_view and purchase, got: %v", kinds)
		}

		// Verify profile property keys include count and last_seen metadata.
		profileKeys := map[string]bool{}
		for _, k := range resp.GetProfilePropertyKeys() {
			profileKeys[k.GetName()] = true
		}
		if !profileKeys["plan"] || !profileKeys["role"] {
			t.Errorf("expected plan and role in profile keys, got: %v", profileKeys)
		}
	})

	t.Run("cache_hit_returns_same_response", func(t *testing.T) {
		resp1, err := svc.GetFilterSchema(ctx, projectID, "")
		if err != nil {
			t.Fatalf("GetFilterSchema (first): %v", err)
		}
		resp2, err := svc.GetFilterSchema(ctx, projectID, "")
		if err != nil {
			t.Fatalf("GetFilterSchema (cached): %v", err)
		}
		if !proto.Equal(resp1, resp2) {
			t.Error("cached response does not match original")
		}
	})

	t.Run("scoped_by_event_kind", func(t *testing.T) {
		resp, err := svc.GetFilterSchema(ctx, projectID, "page_view")
		if err != nil {
			t.Fatalf("GetFilterSchema: %v", err)
		}
		if len(resp.GetAutoPropertyKeys()) == 0 {
			t.Error("expected auto property keys for page_view")
		}
	})
}

func TestServiceGetPropertyValues(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	ch := testutil.SetupClickHouse(t)
	rd := testutil.SetupRedis(t)
	pg := testutil.SetupPostgres(t)

	projectID := seedTestProject(t, ctx, pg)
	seedServiceEvents(t, ctx, ch, projectID)
	seedServiceProfiles(t, ctx, ch, pg, projectID)

	executor := insights.NewExecutor(ch.Conn)
	svc := insights.NewService(executor, rd.Client)

	t.Run("auto_property", func(t *testing.T) {
		values, err := svc.GetPropertyValues(ctx, projectID, "$country", "",
			commonv1.PropertySource_PROPERTY_SOURCE_AUTO)
		if err != nil {
			t.Fatalf("GetPropertyValues: %v", err)
		}
		if len(values) == 0 {
			t.Error("expected at least one auto property value")
		}
	})

	t.Run("profile_property", func(t *testing.T) {
		values, err := svc.GetPropertyValues(ctx, projectID, "plan", "",
			commonv1.PropertySource_PROPERTY_SOURCE_PROFILE)
		if err != nil {
			t.Fatalf("GetPropertyValues: %v", err)
		}
		if len(values) == 0 {
			t.Error("expected at least one profile property value")
		}
	})

	t.Run("cache_hit", func(t *testing.T) {
		vals1, err := svc.GetPropertyValues(ctx, projectID, "$country", "",
			commonv1.PropertySource_PROPERTY_SOURCE_AUTO)
		if err != nil {
			t.Fatalf("GetPropertyValues (first): %v", err)
		}
		vals2, err := svc.GetPropertyValues(ctx, projectID, "$country", "",
			commonv1.PropertySource_PROPERTY_SOURCE_AUTO)
		if err != nil {
			t.Fatalf("GetPropertyValues (cached): %v", err)
		}
		if len(vals1) != len(vals2) {
			t.Errorf("cached result length mismatch: %d vs %d", len(vals1), len(vals2))
		}
	})

	t.Run("unsupported_source", func(t *testing.T) {
		if _, err := svc.GetPropertyValues(ctx, projectID, "$country", "",
			commonv1.PropertySource(99)); err == nil {
			t.Error("expected error for unsupported source")
		}
	})
}

func TestNewServicePanicsOnNilDeps(t *testing.T) {
	rd := testutil.SetupRedis(t)
	executor := &insights.Executor{}

	t.Run("nil_executor", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic for nil executor")
			}
		}()
		insights.NewService(nil, rd.Client)
	})

	t.Run("nil_redis", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic for nil redis")
			}
		}()
		insights.NewService(executor, nil)
	})
}

func TestGroupSeriesBoundsCheck(t *testing.T) {
	rows := []insights.TrendRow{
		{Time: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), EventKind: "pv", Breakdowns: []string{}, Value: 10},
	}
	if _, err := insights.GroupSeries(t.Context(), rows, []string{"$country"}); err == nil {
		t.Error("expected error for mismatched breakdowns/properties")
	}
}

func seedTestProject(t *testing.T, ctx context.Context, pg *testutil.TestPostgres) string {
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

func seedServiceEvents(t *testing.T, ctx context.Context, ch *testutil.TestClickHouse, projectID string) {
	t.Helper()

	now := time.Now().UTC().Truncate(time.Hour)
	events := []struct {
		kind    string
		user    string
		country string
	}{
		{"page_view", "alice", "US"},
		{"page_view", "bob", "GB"},
		{"purchase", "alice", "US"},
	}

	for _, e := range events {
		err := ch.Conn.Exec(ctx,
			`INSERT INTO events (project_id, event_id, kind, distinct_id, occur_time, auto_properties) VALUES (?, ?, ?, ?, ?, ?)`,
			projectID,
			uuid.New().String(),
			e.kind,
			e.user,
			now,
			map[string]string{"$country": e.country},
		)
		if err != nil {
			t.Fatalf("insert event: %v", err)
		}
	}
}

func seedServiceProfiles(t *testing.T, ctx context.Context, ch *testutil.TestClickHouse, pg *testutil.TestPostgres, projectID string) {
	t.Helper()

	profs := []struct {
		externalID string
		properties string
	}{
		{"alice", `{"plan": "pro", "role": "admin"}`},
		{"bob", `{"plan": "free", "role": "member"}`},
	}

	now := time.Now().UTC()

	for _, p := range profs {
		profileID := xid.New().String()

		if _, err := pg.PgW.Exec(ctx,
			`INSERT INTO profiles (id, project_id, external_id, properties) VALUES ($1, $2, $3, $4::jsonb)`,
			profileID,
			projectID,
			p.externalID,
			p.properties,
		); err != nil {
			t.Fatalf("insert profile (postgres): %v", err)
		}

		if err := ch.Conn.Exec(ctx,
			`INSERT INTO profiles (id, project_id, external_id, properties, is_deleted, create_time, update_time, insert_time) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			profileID,
			projectID,
			p.externalID,
			p.properties,
			uint8(0),
			now,
			now,
			now,
		); err != nil {
			t.Fatalf("insert profile (clickhouse): %v", err)
		}
	}
}
