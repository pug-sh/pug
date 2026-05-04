package insights_test

import (
	"context"
	"testing"
	"time"

	chcol "github.com/ClickHouse/clickhouse-go/v2/lib/chcol"
	"github.com/google/uuid"
	"github.com/rs/xid"
	"google.golang.org/protobuf/proto"

	"github.com/pug-sh/pug/internal/core/insights"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	"github.com/pug-sh/pug/internal/testutil"
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
		resp, err := svc.GetFilterSchema(ctx, projectID, "", nil)
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

		// Verify custom property keys include value_type metadata.
		customByName := map[string]commonv1.PropertyValueType{}
		for _, k := range resp.GetCustomPropertyKeys() {
			customByName[k.GetName()] = k.GetValueType()
		}
		expectedTypes := map[string]commonv1.PropertyValueType{
			"load_time":  commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_NUMBER,
			"is_cached":  commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_BOOLEAN,
			"plan_name":  commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_STRING,
			"user_id":    commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_NUMBER,
			"shipped_at": commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_DATETIME,
		}
		for name, want := range expectedTypes {
			if got := customByName[name]; got != want {
				t.Errorf("custom key %q: got value_type %v, want %v", name, got, want)
			}
		}
	})

	t.Run("allowed_types_filters_custom_keys", func(t *testing.T) {
		// NUMBER filter: only load_time (Float64), user_id (Int64), revenue (Float64).
		respNum, err := svc.GetFilterSchema(ctx, projectID, "", []commonv1.PropertyValueType{
			commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_NUMBER,
		})
		if err != nil {
			t.Fatalf("GetFilterSchema (NUMBER): %v", err)
		}
		numKeys := map[string]bool{}
		for _, k := range respNum.GetCustomPropertyKeys() {
			numKeys[k.GetName()] = true
		}
		wantPresent := []string{"load_time", "user_id", "revenue"}
		wantAbsent := []string{"is_cached", "plan_name", "coupon", "shipped_at"}
		for _, name := range wantPresent {
			if !numKeys[name] {
				t.Errorf("NUMBER filter: expected %q in custom keys, got keys: %v", name, numKeys)
			}
		}
		for _, name := range wantAbsent {
			if numKeys[name] {
				t.Errorf("NUMBER filter: unexpected %q in custom keys", name)
			}
		}

		// BOOLEAN filter: only is_cached.
		respBool, err := svc.GetFilterSchema(ctx, projectID, "", []commonv1.PropertyValueType{
			commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_BOOLEAN,
		})
		if err != nil {
			t.Fatalf("GetFilterSchema (BOOLEAN): %v", err)
		}
		boolKeys := map[string]bool{}
		for _, k := range respBool.GetCustomPropertyKeys() {
			boolKeys[k.GetName()] = true
		}
		if !boolKeys["is_cached"] {
			t.Errorf("BOOLEAN filter: expected is_cached in custom keys, got: %v", boolKeys)
		}
		for _, name := range []string{"load_time", "user_id", "plan_name", "coupon", "shipped_at", "revenue"} {
			if boolKeys[name] {
				t.Errorf("BOOLEAN filter: unexpected %q in custom keys", name)
			}
		}

		// DATETIME filter: only shipped_at.
		respDT, err := svc.GetFilterSchema(ctx, projectID, "", []commonv1.PropertyValueType{
			commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_DATETIME,
		})
		if err != nil {
			t.Fatalf("GetFilterSchema (DATETIME): %v", err)
		}
		dtKeys := map[string]bool{}
		for _, k := range respDT.GetCustomPropertyKeys() {
			dtKeys[k.GetName()] = true
		}
		if !dtKeys["shipped_at"] {
			t.Errorf("DATETIME filter: expected shipped_at in custom keys, got: %v", dtKeys)
		}
		for _, name := range []string{"load_time", "user_id", "plan_name", "coupon", "is_cached", "revenue"} {
			if dtKeys[name] {
				t.Errorf("DATETIME filter: unexpected %q in custom keys", name)
			}
		}
	})

	t.Run("cache_hit_returns_same_response", func(t *testing.T) {
		resp1, err := svc.GetFilterSchema(ctx, projectID, "", nil)
		if err != nil {
			t.Fatalf("GetFilterSchema (first): %v", err)
		}
		resp2, err := svc.GetFilterSchema(ctx, projectID, "", nil)
		if err != nil {
			t.Fatalf("GetFilterSchema (cached): %v", err)
		}
		if !proto.Equal(resp1, resp2) {
			t.Error("cached response does not match original")
		}
	})

	t.Run("scoped_by_event_kind", func(t *testing.T) {
		resp, err := svc.GetFilterSchema(ctx, projectID, "page_view", nil)
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

	now := time.Now().UTC().Add(-10 * time.Minute).Truncate(5 * time.Minute)

	type eventRow struct {
		kind   string
		user   string
		auto   map[string]chcol.Variant
		custom map[string]chcol.Variant
	}

	// chcol.NewVariantWithType explicitly tags each value with the ClickHouse
	// Variant branch name, matching the column declaration:
	//   Variant(String, Int64, Float64, Bool, DateTime64(3)).
	// Without explicit type the driver tries each branch in declaration order,
	// which can pick the wrong branch for int64 and time.Time values.
	events := []eventRow{
		{
			kind: "page_view", user: "alice",
			auto: map[string]chcol.Variant{"$country": chcol.NewVariantWithType("US", "String")},
			custom: map[string]chcol.Variant{
				"load_time":  chcol.NewVariantWithType(1.25, "Float64"),
				"is_cached":  chcol.NewVariantWithType(true, "Bool"),
				"plan_name":  chcol.NewVariantWithType("pro", "String"),
				"user_id":    chcol.NewVariantWithType(int64(42), "Int64"),
				"shipped_at": chcol.NewVariantWithType(time.Date(2026, 4, 30, 10, 0, 0, 0, time.UTC), "DateTime64(3)"),
			},
		},
		{
			kind: "page_view", user: "bob",
			auto: map[string]chcol.Variant{"$country": chcol.NewVariantWithType("GB", "String")},
			custom: map[string]chcol.Variant{
				"load_time":  chcol.NewVariantWithType(0.95, "Float64"),
				"is_cached":  chcol.NewVariantWithType(false, "Bool"),
				"plan_name":  chcol.NewVariantWithType("free", "String"),
				"user_id":    chcol.NewVariantWithType(int64(43), "Int64"),
				"shipped_at": chcol.NewVariantWithType(time.Date(2026, 4, 30, 10, 5, 0, 0, time.UTC), "DateTime64(3)"),
			},
		},
		{
			kind: "purchase", user: "alice",
			auto: map[string]chcol.Variant{"$country": chcol.NewVariantWithType("US", "String")},
			custom: map[string]chcol.Variant{
				"revenue": chcol.NewVariantWithType(99.50, "Float64"),
				"coupon":  chcol.NewVariantWithType("SPRING", "String"),
			},
		},
	}

	// Use PrepareBatch (binary native protocol) for Map(String, Variant(...))
	// to ensure the typed Variant branches land correctly. Exec (HTTP) does
	// not reliably carry Variant type discriminators for map values.
	batch, err := ch.Conn.PrepareBatch(ctx,
		"INSERT INTO events (project_id, event_id, kind, distinct_id, occur_time, auto_properties, custom_properties)")
	if err != nil {
		t.Fatalf("prepare batch: %v", err)
	}

	for _, e := range events {
		if err := batch.Append(projectID, uuid.New().String(), e.kind, e.user, now, e.auto, e.custom); err != nil {
			t.Fatalf("append event: %v", err)
		}
	}

	if err := batch.Send(); err != nil {
		t.Fatalf("send event batch: %v", err)
	}

	bucketTime := now
	for _, e := range events {
		for key, v := range e.auto {
			if err := ch.Conn.Exec(ctx,
				`INSERT INTO property_keys_event_buckets (project_id, map_type, kind, bucket_time, key, value_type, event_count, last_seen) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				projectID, "auto", e.kind, bucketTime, key, v.Type(), uint64(1), now,
			); err != nil {
				t.Fatalf("insert auto property key bucket: %v", err)
			}
		}
		for key, v := range e.custom {
			if err := ch.Conn.Exec(ctx,
				`INSERT INTO property_keys_event_buckets (project_id, map_type, kind, bucket_time, key, value_type, event_count, last_seen) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				projectID, "custom", e.kind, bucketTime, key, v.Type(), uint64(1), now,
			); err != nil {
				t.Fatalf("insert custom property key bucket: %v", err)
			}
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

		for _, key := range []string{"plan", "role"} {
			if err := ch.Conn.Exec(ctx,
				`INSERT INTO property_keys_profile_current (project_id, map_type, kind, key, value_type, event_count, last_seen) VALUES (?, ?, ?, ?, ?, ?, ?)`,
				projectID, "profile", "", key, "String", uint64(1), now,
			); err != nil {
				t.Fatalf("insert profile property key current: %v", err)
			}
		}
	}
}
