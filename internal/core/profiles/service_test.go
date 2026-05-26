package profiles_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pug-sh/pug/internal/core/profiles"
	"github.com/pug-sh/pug/internal/testutil"
)

func TestProfilesList_AggregatesAcrossIdentifierKinds(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ch := testutil.SetupClickHouse(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	projectID := "proj-1"

	if err := ch.Conn.Exec(ctx,
		`INSERT INTO profiles (id, project_id, external_id, properties, is_deleted, create_time, update_time) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"user-1", projectID, "ext-1", map[string]any{}, uint8(0), now, now,
	); err != nil {
		t.Fatalf("seed profile: %v", err)
	}

	if err := ch.Conn.Exec(ctx,
		`INSERT INTO profile_aliases (alias_id, profile_id, external_id, project_id) VALUES (?, ?, ?, ?)`,
		"anon-1", "user-1", "ext-1", projectID,
	); err != nil {
		t.Fatalf("seed alias: %v", err)
	}

	// One event per distinct_id kind: canonical id, external_id, alias_id. The
	// activity summary should aggregate all three into the profile's row.
	for _, distinctID := range []string{"user-1", "ext-1", "anon-1"} {
		testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), projectID, distinctID, "page_view", uuid.NewString(),
			map[string]string{},
			map[string]string{},
			now,
		)
	}

	service := profiles.NewService(nil, ch.Conn, nil)
	got, err := service.List(ctx, profiles.ListParams{
		ProjectID: projectID,
		PageSize:  100,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 profile, got %d", len(got))
	}
	p := got[0]
	if p.Activity == nil {
		t.Fatal("expected non-nil Activity")
	}
	if p.Activity.TotalEvents != 3 {
		t.Errorf("TotalEvents = %d, want 3 (events for profile.id + external_id + alias_id)", p.Activity.TotalEvents)
	}
	if p.Activity.Pageviews != 3 {
		t.Errorf("Pageviews = %d, want 3", p.Activity.Pageviews)
	}
	if p.Activity.Sessions != 3 {
		t.Errorf("Sessions = %d, want 3 (one unique session_id per event)", p.Activity.Sessions)
	}
}

func TestProfilesList_NoDoubleCountWhenIDEqualsExternalID(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ch := testutil.SetupClickHouse(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	projectID := "proj-1"

	// Same value for id and external_id — a common SDK convention. The CTE's
	// external_id != p.id guard must prevent the state from being merged twice.
	id := "u-shared"
	if err := ch.Conn.Exec(ctx,
		`INSERT INTO profiles (id, project_id, external_id, properties, is_deleted, create_time, update_time) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, projectID, id, map[string]any{}, uint8(0), now, now,
	); err != nil {
		t.Fatalf("seed profile: %v", err)
	}

	for range 2 {
		testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), projectID, id, "page_view", uuid.NewString(),
			map[string]string{},
			map[string]string{},
			now,
		)
	}

	service := profiles.NewService(nil, ch.Conn, nil)
	got, err := service.List(ctx, profiles.ListParams{
		ProjectID: projectID,
		PageSize:  100,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 profile, got %d", len(got))
	}
	if got[0].Activity == nil {
		t.Fatal("expected non-nil Activity")
	}
	if got[0].Activity.TotalEvents != 2 {
		t.Errorf("TotalEvents = %d, want 2 (no double-count when id == external_id)", got[0].Activity.TotalEvents)
	}
	if got[0].Activity.Pageviews != 2 {
		t.Errorf("Pageviews = %d, want 2", got[0].Activity.Pageviews)
	}
}

// TestProfilesList_DocumentedInflationWhenExternalIDEqualsAliasID pins the
// documented edge case in profileActivitySummaryCTE: when a profile's
// external_id coincides with one of its alias_ids, the additive aggregates
// (total_events, pageviews) double-count, while the idempotent aggregates
// (sessions HyperLogLog, argMax columns) remain correct. A future change
// that adds an external_id != any-alias_id guard will trip this test loudly
// rather than silently change values.
func TestProfilesList_DocumentedInflationWhenExternalIDEqualsAliasID(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ch := testutil.SetupClickHouse(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	projectID := "proj-1"

	// Profile whose external_id collides with one of its alias_ids.
	if err := ch.Conn.Exec(ctx,
		`INSERT INTO profiles (id, project_id, external_id, properties, is_deleted, create_time, update_time) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"u", projectID, "shared-id", map[string]any{}, uint8(0), now, now,
	); err != nil {
		t.Fatalf("seed profile: %v", err)
	}

	if err := ch.Conn.Exec(ctx,
		`INSERT INTO profile_aliases (alias_id, profile_id, external_id, project_id) VALUES (?, ?, ?, ?)`,
		"shared-id", "u", "shared-id", projectID,
	); err != nil {
		t.Fatalf("seed alias: %v", err)
	}

	// One event keyed by the colliding identifier. The UNION's external_id
	// branch and alias branch both emit (project, profile, "shared-id"),
	// joining the same states row twice.
	sessionID := uuid.NewString()
	testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), projectID, "shared-id", "page_view", sessionID,
		map[string]string{"$browser": "Chrome", "$country": "US"},
		map[string]string{},
		now,
	)

	service := profiles.NewService(nil, ch.Conn, nil)
	got, err := service.List(ctx, profiles.ListParams{
		ProjectID: projectID,
		PageSize:  100,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 profile, got %d", len(got))
	}
	a := got[0].Activity
	if a == nil {
		t.Fatal("expected non-nil Activity")
	}

	// Additive aggregates double-count.
	if a.TotalEvents != 2 {
		t.Errorf("TotalEvents = %d, want 2 (documented double-count when external_id == alias_id)", a.TotalEvents)
	}
	if a.Pageviews != 2 {
		t.Errorf("Pageviews = %d, want 2 (documented double-count)", a.Pageviews)
	}

	// HyperLogLog and argMax aggregates are idempotent under repeated merge.
	if a.Sessions != 1 {
		t.Errorf("Sessions = %d, want 1 (HyperLogLog idempotent under duplicate merge)", a.Sessions)
	}
	if a.Browser != "Chrome" {
		t.Errorf("Browser = %q, want \"Chrome\" (argMax idempotent under duplicate merge)", a.Browser)
	}
	if a.Country != "US" {
		t.Errorf("Country = %q, want \"US\" (argMax idempotent under duplicate merge)", a.Country)
	}
}
