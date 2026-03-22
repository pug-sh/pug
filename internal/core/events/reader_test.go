package events_test

import (
	"context"
	"testing"
	"time"

	"github.com/fivebitsio/cotton/internal/core/events"
	"github.com/fivebitsio/cotton/internal/testutil"
	"github.com/google/uuid"
)

func TestEventsReader(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ch := testutil.SetupClickHouse(t)
	ctx := context.Background()

	now := time.Now().Truncate(time.Second)

	// Seed events for user-1.
	for i, kind := range []string{"page_view", "purchase", "signup"} {
		err := ch.Conn.Exec(ctx,
			`INSERT INTO events (event_id, project_id, distinct_id, kind, auto_properties, custom_properties, occur_time) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			uuid.NewString(), "proj-1", "user-1", kind,
			map[string]string{"$country": "US"},
			map[string]string{},
			now.Add(time.Duration(-i)*time.Minute),
		)
		if err != nil {
			t.Fatalf("seed event %s: %v", kind, err)
		}
	}

	// Seed events for anon-1 (will be aliased to user-1).
	err := ch.Conn.Exec(ctx,
		`INSERT INTO events (event_id, project_id, distinct_id, kind, auto_properties, custom_properties, occur_time) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		uuid.NewString(), "proj-1", "anon-1", "anonymous_action",
		map[string]string{},
		map[string]string{},
		now.Add(-5*time.Minute),
	)
	if err != nil {
		t.Fatalf("seed anon event: %v", err)
	}

	// Seed alias: anon-1 → user-1.
	err = ch.Conn.Exec(ctx,
		`INSERT INTO profile_aliases (alias_id, profile_id, external_id, project_id) VALUES (?, ?, ?, ?)`,
		"anon-1", "user-1", "ext-1", "proj-1",
	)
	if err != nil {
		t.Fatalf("seed alias: %v", err)
	}

	// Seed event for different project (should not appear).
	err = ch.Conn.Exec(ctx,
		`INSERT INTO events (event_id, project_id, distinct_id, kind, auto_properties, custom_properties, occur_time) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		uuid.NewString(), "proj-2", "user-1", "other_project_event",
		map[string]string{},
		map[string]string{},
		now,
	)
	if err != nil {
		t.Fatalf("seed other project event: %v", err)
	}

	reader := events.NewReader(ch.Conn)

	t.Run("returns events for profile", func(t *testing.T) {
		evts, err := reader.GetEventsByProfile(ctx, "proj-1", "user-1")
		if err != nil {
			t.Fatalf("GetEventsByProfile: %v", err)
		}
		// 3 events for user-1 + 1 for anon-1 (alias) = 4
		if len(evts) != 4 {
			t.Fatalf("expected 4 events, got %d", len(evts))
		}
	})

	t.Run("ordered by occur_time DESC", func(t *testing.T) {
		evts, err := reader.GetEventsByProfile(ctx, "proj-1", "user-1")
		if err != nil {
			t.Fatalf("GetEventsByProfile: %v", err)
		}
		for i := 1; i < len(evts); i++ {
			if evts[i].OccurTime.After(evts[i-1].OccurTime) {
				t.Errorf("events not ordered DESC: [%d].OccurTime=%v > [%d].OccurTime=%v",
					i, evts[i].OccurTime, i-1, evts[i-1].OccurTime)
			}
		}
	})

	t.Run("includes alias events", func(t *testing.T) {
		evts, err := reader.GetEventsByProfile(ctx, "proj-1", "user-1")
		if err != nil {
			t.Fatalf("GetEventsByProfile: %v", err)
		}
		found := false
		for _, e := range evts {
			if e.Kind == "anonymous_action" && e.DistinctID == "anon-1" {
				found = true
				break
			}
		}
		if !found {
			t.Error("expected to find aliased anon-1 event in user-1 results")
		}
	})

	t.Run("empty for nonexistent profile", func(t *testing.T) {
		evts, err := reader.GetEventsByProfile(ctx, "proj-1", "nonexistent")
		if err != nil {
			t.Fatalf("GetEventsByProfile: %v", err)
		}
		if len(evts) != 0 {
			t.Errorf("expected 0 events for nonexistent profile, got %d", len(evts))
		}
	})

	t.Run("scoped to project", func(t *testing.T) {
		evts, err := reader.GetEventsByProfile(ctx, "proj-2", "user-1")
		if err != nil {
			t.Fatalf("GetEventsByProfile (proj-2): %v", err)
		}
		if len(evts) != 1 {
			t.Errorf("expected 1 event for proj-2/user-1, got %d", len(evts))
		}
	})
}
