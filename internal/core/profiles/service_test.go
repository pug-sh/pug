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
		if err := ch.Conn.Exec(ctx,
			`INSERT INTO events (event_id, project_id, distinct_id, kind, auto_properties, custom_properties, occur_time, session_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			uuid.NewString(), projectID, distinctID, "page_view",
			map[string]string{},
			map[string]string{},
			now,
			uuid.NewString(),
		); err != nil {
			t.Fatalf("seed event for distinct_id %q: %v", distinctID, err)
		}
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

	for i := range 2 {
		if err := ch.Conn.Exec(ctx,
			`INSERT INTO events (event_id, project_id, distinct_id, kind, auto_properties, custom_properties, occur_time, session_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			uuid.NewString(), projectID, id, "page_view",
			map[string]string{},
			map[string]string{},
			now,
			uuid.NewString(),
		); err != nil {
			t.Fatalf("seed event %d: %v", i, err)
		}
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
