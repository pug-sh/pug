package events_test

import (
	"context"
	"testing"
	"time"

	"github.com/fivebitsio/cotton/internal/core/events"
	commonv1 "github.com/fivebitsio/cotton/internal/gen/proto/common/v1"
	"github.com/fivebitsio/cotton/internal/testutil"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestGetActivityFeed(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ch := testutil.SetupClickHouse(t)
	ctx := context.Background()

	now := time.Now().Truncate(time.Second)
	sessionA := uuid.NewString()
	sessionB := uuid.NewString()

	seedEvents := []struct {
		kind      string
		offset    time.Duration
		sessionID string
	}{
		{"page_view", 0, sessionA},
		{"purchase", -1 * time.Minute, sessionA},
		{"signup", -2 * time.Minute, sessionA},
		{"page_view", -3 * time.Minute, sessionB},
		{"logout", -4 * time.Minute, sessionB},
	}
	for _, se := range seedEvents {
		eid := uuid.NewString()
		err := ch.Conn.Exec(ctx,
			`INSERT INTO events (event_id, project_id, distinct_id, kind, auto_properties, custom_properties, occur_time, session_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			eid, "proj-1", "user-1", se.kind,
			map[string]string{"$country": "US"},
			map[string]string{"plan": "pro"},
			now.Add(se.offset),
			se.sessionID,
		)
		if err != nil {
			t.Fatalf("seed event %s: %v", se.kind, err)
		}
	}

	err := ch.Conn.Exec(ctx,
		`INSERT INTO profile_aliases (alias_id, profile_id, external_id, project_id) VALUES (?, ?, ?, ?)`,
		"anon-1", "user-1", "ext-1", "proj-1",
	)
	if err != nil {
		t.Fatalf("seed alias: %v", err)
	}

	anonEID := uuid.NewString()
	err = ch.Conn.Exec(ctx,
		`INSERT INTO events (event_id, project_id, distinct_id, kind, auto_properties, custom_properties, occur_time, session_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		anonEID, "proj-1", "anon-1", "anon_action",
		map[string]string{},
		map[string]string{},
		now.Add(-5*time.Minute),
		sessionB,
	)
	if err != nil {
		t.Fatalf("seed anon event: %v", err)
	}

	// Seed event for different project (should not appear in proj-1 queries).
	err = ch.Conn.Exec(ctx,
		`INSERT INTO events (event_id, project_id, distinct_id, kind, auto_properties, custom_properties, occur_time, session_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		uuid.NewString(), "proj-2", "user-1", "other_project_event",
		map[string]string{},
		map[string]string{},
		now,
		uuid.NewString(),
	)
	if err != nil {
		t.Fatalf("seed other project event: %v", err)
	}

	reader := events.NewReader(ch.Conn)

	t.Run("returns all events with alias resolution", func(t *testing.T) {
		evts, cursor, err := reader.GetActivityFeed(ctx, events.ActivityFeedParams{
			ProjectID:  "proj-1",
			DistinctID: "user-1",
			PageSize:   100,
		})
		if err != nil {
			t.Fatalf("GetActivityFeed: %v", err)
		}
		if len(evts) != 6 {
			t.Fatalf("expected 6 events, got %d", len(evts))
		}
		if cursor != nil {
			t.Errorf("expected nil cursor, got %+v", cursor)
		}
	})

	t.Run("ordered by occur_time DESC", func(t *testing.T) {
		evts, _, err := reader.GetActivityFeed(ctx, events.ActivityFeedParams{
			ProjectID:  "proj-1",
			DistinctID: "user-1",
			PageSize:   100,
		})
		if err != nil {
			t.Fatalf("GetActivityFeed: %v", err)
		}
		for i := 1; i < len(evts); i++ {
			if evts[i].OccurTime.After(evts[i-1].OccurTime) {
				t.Errorf("not ordered DESC: [%d]=%v > [%d]=%v", i, evts[i].OccurTime, i-1, evts[i-1].OccurTime)
			}
		}
	})

	t.Run("filters by kind", func(t *testing.T) {
		evts, _, err := reader.GetActivityFeed(ctx, events.ActivityFeedParams{
			ProjectID:  "proj-1",
			DistinctID: "user-1",
			EventFilters:     []*commonv1.EventFilter{{Kind: "page_view"}},
			PageSize:   100,
		})
		if err != nil {
			t.Fatalf("GetActivityFeed: %v", err)
		}
		if len(evts) != 2 {
			t.Fatalf("expected 2 page_view events, got %d", len(evts))
		}
	})

	t.Run("filters by session_id", func(t *testing.T) {
		evts, _, err := reader.GetActivityFeed(ctx, events.ActivityFeedParams{
			ProjectID:  "proj-1",
			DistinctID: "user-1",
			SessionID:  sessionA,
			PageSize:   100,
		})
		if err != nil {
			t.Fatalf("GetActivityFeed: %v", err)
		}
		if len(evts) != 3 {
			t.Fatalf("expected 3 events for session A, got %d", len(evts))
		}
	})

	t.Run("pagination", func(t *testing.T) {
		evts1, cursor1, err := reader.GetActivityFeed(ctx, events.ActivityFeedParams{
			ProjectID:  "proj-1",
			DistinctID: "user-1",
			PageSize:   3,
		})
		if err != nil {
			t.Fatalf("page 1: %v", err)
		}
		if len(evts1) != 3 {
			t.Fatalf("page 1: expected 3, got %d", len(evts1))
		}
		if cursor1 == nil {
			t.Fatal("page 1: expected cursor")
		}

		evts2, cursor2, err := reader.GetActivityFeed(ctx, events.ActivityFeedParams{
			ProjectID:  "proj-1",
			DistinctID: "user-1",
			PageSize:   3,
			PageToken:  cursor1,
		})
		if err != nil {
			t.Fatalf("page 2: %v", err)
		}
		if len(evts2) != 3 {
			t.Fatalf("page 2: expected 3, got %d", len(evts2))
		}

		evts3, _, err := reader.GetActivityFeed(ctx, events.ActivityFeedParams{
			ProjectID:  "proj-1",
			DistinctID: "user-1",
			PageSize:   3,
			PageToken:  cursor2,
		})
		if err != nil {
			t.Fatalf("page 3: %v", err)
		}
		if len(evts3) != 0 {
			t.Fatalf("page 3: expected 0, got %d", len(evts3))
		}

		seen := make(map[string]bool)
		for _, e := range evts1 {
			seen[e.EventID] = true
		}
		for _, e := range evts2 {
			if seen[e.EventID] {
				t.Errorf("duplicate event %s across pages", e.EventID)
			}
		}
	})

	t.Run("default page size", func(t *testing.T) {
		evts, _, err := reader.GetActivityFeed(ctx, events.ActivityFeedParams{
			ProjectID:  "proj-1",
			DistinctID: "user-1",
		})
		if err != nil {
			t.Fatalf("GetActivityFeed: %v", err)
		}
		if len(evts) != 6 {
			t.Fatalf("expected 6 events with default page size, got %d", len(evts))
		}
	})

	t.Run("negative page size defaults to 100", func(t *testing.T) {
		evts, _, err := reader.GetActivityFeed(ctx, events.ActivityFeedParams{
			ProjectID:  "proj-1",
			DistinctID: "user-1",
			PageSize:   -5,
		})
		if err != nil {
			t.Fatalf("GetActivityFeed: %v", err)
		}
		// Default 100 is applied, we only have 6 events
		if len(evts) != 6 {
			t.Fatalf("expected 6 events with negative page size, got %d", len(evts))
		}
	})

	t.Run("page size capped at max", func(t *testing.T) {
		evts, _, err := reader.GetActivityFeed(ctx, events.ActivityFeedParams{
			ProjectID:  "proj-1",
			DistinctID: "user-1",
			PageSize:   5000,
		})
		if err != nil {
			t.Fatalf("GetActivityFeed: %v", err)
		}
		// Capped to 1000, we only have 6 events so all returned
		if len(evts) != 6 {
			t.Fatalf("expected 6 events with capped page size, got %d", len(evts))
		}
	})

	t.Run("empty for nonexistent profile", func(t *testing.T) {
		evts, cursor, err := reader.GetActivityFeed(ctx, events.ActivityFeedParams{
			ProjectID:  "proj-1",
			DistinctID: "nonexistent",
			PageSize:   100,
		})
		if err != nil {
			t.Fatalf("GetActivityFeed: %v", err)
		}
		if len(evts) != 0 {
			t.Errorf("expected 0 events for nonexistent profile, got %d", len(evts))
		}
		if cursor != nil {
			t.Errorf("expected nil cursor for empty results, got %+v", cursor)
		}
	})

	t.Run("pagination ordering continuity", func(t *testing.T) {
		evts1, cursor1, err := reader.GetActivityFeed(ctx, events.ActivityFeedParams{
			ProjectID:  "proj-1",
			DistinctID: "user-1",
			PageSize:   3,
		})
		if err != nil {
			t.Fatalf("page 1: %v", err)
		}
		if cursor1 == nil {
			t.Fatal("page 1: expected cursor")
		}

		evts2, _, err := reader.GetActivityFeed(ctx, events.ActivityFeedParams{
			ProjectID:  "proj-1",
			DistinctID: "user-1",
			PageSize:   3,
			PageToken:  cursor1,
		})
		if err != nil {
			t.Fatalf("page 2: %v", err)
		}

		// Page 2's first event must not be after page 1's last event
		if len(evts1) > 0 && len(evts2) > 0 {
			lastP1 := evts1[len(evts1)-1]
			firstP2 := evts2[0]
			if firstP2.OccurTime.After(lastP1.OccurTime) {
				t.Errorf("page 2 starts after page 1 ends: p2[0]=%v > p1[last]=%v",
					firstP2.OccurTime, lastP1.OccurTime)
			}
		}
	})

	t.Run("pagination via encoded token", func(t *testing.T) {
		// Test the full encode→string→decode round-trip as used by the RPC handler
		evts1, cursor1, err := reader.GetActivityFeed(ctx, events.ActivityFeedParams{
			ProjectID:  "proj-1",
			DistinctID: "user-1",
			PageSize:   3,
		})
		if err != nil {
			t.Fatalf("page 1: %v", err)
		}
		if cursor1 == nil {
			t.Fatal("page 1: expected cursor")
		}

		// Encode to string and decode back (simulates what the handler does)
		token, err := cursor1.Encode()
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		decoded, err := events.DecodeActivityFeedCursor(token)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}

		evts2, _, err := reader.GetActivityFeed(ctx, events.ActivityFeedParams{
			ProjectID:  "proj-1",
			DistinctID: "user-1",
			PageSize:   3,
			PageToken:  decoded,
		})
		if err != nil {
			t.Fatalf("page 2: %v", err)
		}
		if len(evts2) != 3 {
			t.Fatalf("page 2: expected 3, got %d", len(evts2))
		}

		// No overlap with page 1
		seen := make(map[string]bool)
		for _, e := range evts1 {
			seen[e.EventID] = true
		}
		for _, e := range evts2 {
			if seen[e.EventID] {
				t.Errorf("duplicate event %s across encode/decode pagination", e.EventID)
			}
		}
	})

	t.Run("filters by time_range", func(t *testing.T) {
		// Time range covers only the first 2 events (now and now-1min)
		evts, _, err := reader.GetActivityFeed(ctx, events.ActivityFeedParams{
			ProjectID:  "proj-1",
			DistinctID: "user-1",
			PageSize:   100,
			TimeRange: &commonv1.TimeRange{
				From: timestamppb.New(now.Add(-90 * time.Second)),
				To:   timestamppb.New(now.Add(time.Second)),
			},
		})
		if err != nil {
			t.Fatalf("GetActivityFeed: %v", err)
		}
		if len(evts) != 2 {
			t.Fatalf("expected 2 events in time range, got %d", len(evts))
		}
		for _, e := range evts {
			if e.OccurTime.Before(now.Add(-90*time.Second)) || !e.OccurTime.Before(now.Add(time.Second)) {
				t.Errorf("event %s occur_time %v outside time range", e.EventID, e.OccurTime)
			}
		}
	})

	t.Run("filters by property", func(t *testing.T) {
		// Filter for $country = US — only user-1 events have this, not anon-1
		evts, _, err := reader.GetActivityFeed(ctx, events.ActivityFeedParams{
			ProjectID:  "proj-1",
			DistinctID: "user-1",
			PageSize:   100,
			PropertyFilters: []*commonv1.PropertyFilter{
				{
					Property: "$country",
					Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS,
					Value:    "US",
				},
			},
		})
		if err != nil {
			t.Fatalf("GetActivityFeed: %v", err)
		}
		// 5 user-1 events have $country=US, anon-1 does not
		if len(evts) != 5 {
			t.Fatalf("expected 5 events with $country=US, got %d", len(evts))
		}
	})

	t.Run("filters by custom property", func(t *testing.T) {
		// Filter for plan = pro — only user-1 events have this
		evts, _, err := reader.GetActivityFeed(ctx, events.ActivityFeedParams{
			ProjectID:  "proj-1",
			DistinctID: "user-1",
			PageSize:   100,
			PropertyFilters: []*commonv1.PropertyFilter{
				{
					Property: "plan",
					Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS,
					Value:    "pro",
				},
			},
		})
		if err != nil {
			t.Fatalf("GetActivityFeed: %v", err)
		}
		if len(evts) != 5 {
			t.Fatalf("expected 5 events with plan=pro, got %d", len(evts))
		}
	})

	t.Run("scoped to project", func(t *testing.T) {
		evts, _, err := reader.GetActivityFeed(ctx, events.ActivityFeedParams{
			ProjectID:  "proj-2",
			DistinctID: "user-1",
			PageSize:   100,
		})
		if err != nil {
			t.Fatalf("GetActivityFeed (proj-2): %v", err)
		}
		if len(evts) != 1 {
			t.Errorf("expected 1 event for proj-2/user-1, got %d", len(evts))
		}
	})

	t.Run("combined filters", func(t *testing.T) {
		// kind=page_view + sessionA + time range covering first 2 events + $country=US
		// Only the first page_view (now, sessionA) matches all criteria
		evts, _, err := reader.GetActivityFeed(ctx, events.ActivityFeedParams{
			ProjectID:  "proj-1",
			DistinctID: "user-1",
			EventFilters:     []*commonv1.EventFilter{{Kind: "page_view"}},
			SessionID:  sessionA,
			PageSize:   100,
			TimeRange: &commonv1.TimeRange{
				From: timestamppb.New(now.Add(-90 * time.Second)),
				To:   timestamppb.New(now.Add(time.Second)),
			},
			PropertyFilters: []*commonv1.PropertyFilter{
				{
					Property: "$country",
					Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS,
					Value:    "US",
				},
			},
		})
		if err != nil {
			t.Fatalf("GetActivityFeed: %v", err)
		}
		// page_view at now (sessionA, in time range, $country=US) = 1 match
		if len(evts) != 1 {
			t.Fatalf("expected 1 event with combined filters, got %d", len(evts))
		}
		if evts[0].Kind != "page_view" {
			t.Errorf("expected kind page_view, got %s", evts[0].Kind)
		}
	})

	t.Run("multi-event filters", func(t *testing.T) {
		evts, _, err := reader.GetActivityFeed(ctx, events.ActivityFeedParams{
			ProjectID:  "proj-1",
			DistinctID: "user-1",
			EventFilters: []*commonv1.EventFilter{
				{Kind: "page_view"},
				{Kind: "purchase"},
			},
			PageSize: 100,
		})
		if err != nil {
			t.Fatalf("GetActivityFeed: %v", err)
		}
		// 2 page_view + 1 purchase = 3
		if len(evts) != 3 {
			t.Fatalf("expected 3 events for multi-event filter, got %d", len(evts))
		}
		for _, e := range evts {
			if e.Kind != "page_view" && e.Kind != "purchase" {
				t.Errorf("unexpected kind %s", e.Kind)
			}
		}
	})

	t.Run("multi-event with per-event filters", func(t *testing.T) {
		evts, _, err := reader.GetActivityFeed(ctx, events.ActivityFeedParams{
			ProjectID:  "proj-1",
			DistinctID: "user-1",
			EventFilters: []*commonv1.EventFilter{
				{
					Kind: "page_view",
					Filters: []*commonv1.PropertyFilter{
						{Property: "$country", Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS, Value: "US"},
					},
				},
				{Kind: "signup"},
			},
			PageSize: 100,
		})
		if err != nil {
			t.Fatalf("GetActivityFeed: %v", err)
		}
		// 2 page_view (all have $country=US) + 1 signup = 3
		if len(evts) != 3 {
			t.Fatalf("expected 3 events, got %d", len(evts))
		}
	})
}

func TestGetEventExplorer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ch := testutil.SetupClickHouse(t)
	ctx := context.Background()

	now := time.Now().Truncate(time.Second)
	sessionA := uuid.NewString()
	sessionB := uuid.NewString()

	// Seed events for multiple users in proj-1.
	seedData := []struct {
		distinctID string
		kind       string
		offset     time.Duration
		sessionID  string
		country    string
	}{
		{"user-1", "page_view", 0, sessionA, "US"},
		{"user-1", "purchase", -1 * time.Minute, sessionA, "US"},
		{"user-2", "page_view", -2 * time.Minute, sessionB, "DE"},
		{"user-2", "signup", -3 * time.Minute, sessionB, "DE"},
		{"user-3", "page_view", -4 * time.Minute, sessionA, "US"},
	}
	for _, se := range seedData {
		err := ch.Conn.Exec(ctx,
			`INSERT INTO events (event_id, project_id, distinct_id, kind, auto_properties, custom_properties, occur_time, session_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			uuid.NewString(), "proj-1", se.distinctID, se.kind,
			map[string]string{"$country": se.country},
			map[string]string{},
			now.Add(se.offset),
			se.sessionID,
		)
		if err != nil {
			t.Fatalf("seed event: %v", err)
		}
	}

	// Seed alias: anon-1 → user-1 (should NOT be resolved by event explorer).
	err := ch.Conn.Exec(ctx,
		`INSERT INTO profile_aliases (alias_id, profile_id, external_id, project_id) VALUES (?, ?, ?, ?)`,
		"anon-1", "user-1", "ext-1", "proj-1",
	)
	if err != nil {
		t.Fatalf("seed alias: %v", err)
	}

	// Seed event under anon-1.
	err = ch.Conn.Exec(ctx,
		`INSERT INTO events (event_id, project_id, distinct_id, kind, auto_properties, custom_properties, occur_time, session_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		uuid.NewString(), "proj-1", "anon-1", "anon_action",
		map[string]string{},
		map[string]string{},
		now.Add(-5*time.Minute),
		sessionB,
	)
	if err != nil {
		t.Fatalf("seed anon event: %v", err)
	}

	// Seed event for different project.
	err = ch.Conn.Exec(ctx,
		`INSERT INTO events (event_id, project_id, distinct_id, kind, auto_properties, custom_properties, occur_time, session_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		uuid.NewString(), "proj-2", "user-1", "other_project",
		map[string]string{},
		map[string]string{},
		now,
		uuid.NewString(),
	)
	if err != nil {
		t.Fatalf("seed other project event: %v", err)
	}

	reader := events.NewReader(ch.Conn)

	t.Run("returns all project events without distinct_id", func(t *testing.T) {
		evts, _, err := reader.GetEventExplorer(ctx, events.EventExplorerParams{
			ProjectID: "proj-1",
			PageSize:  100,
		})
		if err != nil {
			t.Fatalf("GetEventExplorer: %v", err)
		}
		// 5 seeded + 1 anon = 6 events in proj-1
		if len(evts) != 6 {
			t.Fatalf("expected 6 events, got %d", len(evts))
		}
	})

	t.Run("filters by optional distinct_id", func(t *testing.T) {
		distinctID := "user-2"
		evts, _, err := reader.GetEventExplorer(ctx, events.EventExplorerParams{
			ProjectID:  "proj-1",
			DistinctID: distinctID,
			PageSize:   100,
		})
		if err != nil {
			t.Fatalf("GetEventExplorer: %v", err)
		}
		if len(evts) != 2 {
			t.Fatalf("expected 2 events for user-2, got %d", len(evts))
		}
		for _, e := range evts {
			if e.DistinctID != "user-2" {
				t.Errorf("expected distinct_id user-2, got %s", e.DistinctID)
			}
		}
	})

	t.Run("does not resolve aliases", func(t *testing.T) {
		// Query for user-1 — should NOT include anon-1 events
		distinctID := "user-1"
		evts, _, err := reader.GetEventExplorer(ctx, events.EventExplorerParams{
			ProjectID:  "proj-1",
			DistinctID: distinctID,
			PageSize:   100,
		})
		if err != nil {
			t.Fatalf("GetEventExplorer: %v", err)
		}
		// Only 2 user-1 events (not 3 with alias)
		if len(evts) != 2 {
			t.Fatalf("expected 2 events for user-1 (no alias resolution), got %d", len(evts))
		}
		for _, e := range evts {
			if e.DistinctID != "user-1" {
				t.Errorf("expected distinct_id user-1, got %s", e.DistinctID)
			}
		}
	})

	t.Run("scoped to project", func(t *testing.T) {
		evts, _, err := reader.GetEventExplorer(ctx, events.EventExplorerParams{
			ProjectID: "proj-2",
			PageSize:  100,
		})
		if err != nil {
			t.Fatalf("GetEventExplorer: %v", err)
		}
		if len(evts) != 1 {
			t.Errorf("expected 1 event for proj-2, got %d", len(evts))
		}
	})

	t.Run("ordered by occur_time DESC", func(t *testing.T) {
		evts, _, err := reader.GetEventExplorer(ctx, events.EventExplorerParams{
			ProjectID: "proj-1",
			PageSize:  100,
		})
		if err != nil {
			t.Fatalf("GetEventExplorer: %v", err)
		}
		for i := 1; i < len(evts); i++ {
			if evts[i].OccurTime.After(evts[i-1].OccurTime) {
				t.Errorf("not ordered DESC: [%d]=%v > [%d]=%v", i, evts[i].OccurTime, i-1, evts[i-1].OccurTime)
			}
		}
	})

	t.Run("filters by kind", func(t *testing.T) {
		evts, _, err := reader.GetEventExplorer(ctx, events.EventExplorerParams{
			ProjectID: "proj-1",
			EventFilters:    []*commonv1.EventFilter{{Kind: "page_view"}},
			PageSize:  100,
		})
		if err != nil {
			t.Fatalf("GetEventExplorer: %v", err)
		}
		// user-1, user-2, user-3 each have one page_view = 3
		if len(evts) != 3 {
			t.Fatalf("expected 3 page_view events, got %d", len(evts))
		}
	})

	t.Run("filters by session_id", func(t *testing.T) {
		evts, _, err := reader.GetEventExplorer(ctx, events.EventExplorerParams{
			ProjectID: "proj-1",
			SessionID: sessionA,
			PageSize:  100,
		})
		if err != nil {
			t.Fatalf("GetEventExplorer: %v", err)
		}
		// sessionA: user-1 page_view, user-1 purchase, user-3 page_view = 3
		if len(evts) != 3 {
			t.Fatalf("expected 3 events for session A, got %d", len(evts))
		}
	})

	t.Run("filters by time_range", func(t *testing.T) {
		evts, _, err := reader.GetEventExplorer(ctx, events.EventExplorerParams{
			ProjectID: "proj-1",
			PageSize:  100,
			TimeRange: &commonv1.TimeRange{
				From: timestamppb.New(now.Add(-90 * time.Second)),
				To:   timestamppb.New(now.Add(time.Second)),
			},
		})
		if err != nil {
			t.Fatalf("GetEventExplorer: %v", err)
		}
		// now (user-1 page_view) and now-1min (user-1 purchase) = 2
		if len(evts) != 2 {
			t.Fatalf("expected 2 events in time range, got %d", len(evts))
		}
	})

	t.Run("filters by property", func(t *testing.T) {
		evts, _, err := reader.GetEventExplorer(ctx, events.EventExplorerParams{
			ProjectID: "proj-1",
			PageSize:  100,
			PropertyFilters: []*commonv1.PropertyFilter{
				{
					Property: "$country",
					Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS,
					Value:    "DE",
				},
			},
		})
		if err != nil {
			t.Fatalf("GetEventExplorer: %v", err)
		}
		// user-2 has 2 events with $country=DE
		if len(evts) != 2 {
			t.Fatalf("expected 2 events with $country=DE, got %d", len(evts))
		}
	})

	t.Run("pagination", func(t *testing.T) {
		evts1, cursor1, err := reader.GetEventExplorer(ctx, events.EventExplorerParams{
			ProjectID: "proj-1",
			PageSize:  3,
		})
		if err != nil {
			t.Fatalf("page 1: %v", err)
		}
		if len(evts1) != 3 {
			t.Fatalf("page 1: expected 3, got %d", len(evts1))
		}
		if cursor1 == nil {
			t.Fatal("page 1: expected cursor")
		}

		evts2, cursor2, err := reader.GetEventExplorer(ctx, events.EventExplorerParams{
			ProjectID: "proj-1",
			PageSize:  3,
			PageToken: cursor1,
		})
		if err != nil {
			t.Fatalf("page 2: %v", err)
		}
		if len(evts2) != 3 {
			t.Fatalf("page 2: expected 3, got %d", len(evts2))
		}

		evts3, _, err := reader.GetEventExplorer(ctx, events.EventExplorerParams{
			ProjectID: "proj-1",
			PageSize:  3,
			PageToken: cursor2,
		})
		if err != nil {
			t.Fatalf("page 3: %v", err)
		}
		if len(evts3) != 0 {
			t.Fatalf("page 3: expected 0, got %d", len(evts3))
		}

		// No overlap
		seen := make(map[string]bool)
		for _, e := range evts1 {
			seen[e.EventID] = true
		}
		for _, e := range evts2 {
			if seen[e.EventID] {
				t.Errorf("duplicate event %s across pages", e.EventID)
			}
		}
	})

	t.Run("combined filters", func(t *testing.T) {
		evts, _, err := reader.GetEventExplorer(ctx, events.EventExplorerParams{
			ProjectID: "proj-1",
			EventFilters:    []*commonv1.EventFilter{{Kind: "page_view"}},
			SessionID: sessionA,
			PageSize:  100,
			TimeRange: &commonv1.TimeRange{
				From: timestamppb.New(now.Add(-90 * time.Second)),
				To:   timestamppb.New(now.Add(time.Second)),
			},
			PropertyFilters: []*commonv1.PropertyFilter{
				{
					Property: "$country",
					Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS,
					Value:    "US",
				},
			},
		})
		if err != nil {
			t.Fatalf("GetEventExplorer: %v", err)
		}
		// page_view + sessionA + time range (now to now-90s) + $country=US = user-1 page_view only
		if len(evts) != 1 {
			t.Fatalf("expected 1 event with combined filters, got %d", len(evts))
		}
	})

	t.Run("multi-event filters", func(t *testing.T) {
		evts, _, err := reader.GetEventExplorer(ctx, events.EventExplorerParams{
			ProjectID: "proj-1",
			EventFilters: []*commonv1.EventFilter{
				{Kind: "page_view"},
				{Kind: "purchase"},
			},
			PageSize: 100,
		})
		if err != nil {
			t.Fatalf("GetEventExplorer: %v", err)
		}
		// 3 page_view (one per user) + 1 purchase = 4
		if len(evts) != 4 {
			t.Fatalf("expected 4 events for multi-event filter, got %d", len(evts))
		}
		for _, e := range evts {
			if e.Kind != "page_view" && e.Kind != "purchase" {
				t.Errorf("unexpected kind %s", e.Kind)
			}
		}
	})

	t.Run("multi-event with per-event filters", func(t *testing.T) {
		evts, _, err := reader.GetEventExplorer(ctx, events.EventExplorerParams{
			ProjectID: "proj-1",
			EventFilters: []*commonv1.EventFilter{
				{
					Kind: "page_view",
					Filters: []*commonv1.PropertyFilter{
						{Property: "$country", Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS, Value: "US"},
					},
				},
				{Kind: "signup"},
			},
			PageSize: 100,
		})
		if err != nil {
			t.Fatalf("GetEventExplorer: %v", err)
		}
		// page_view with $country=US: user-1 (US) + user-3 (US) = 2. user-2 has $country=DE, excluded.
		// signup: user-2 = 1. Total = 3.
		if len(evts) != 3 {
			t.Fatalf("expected 3 events, got %d", len(evts))
		}
	})
}

func TestActivityFeedCursor_RoundTrip(t *testing.T) {
	original := &events.ActivityFeedCursor{
		OccurTime: time.Date(2026, 3, 26, 12, 0, 0, 0, time.UTC),
		EventID:   "evt-123",
	}

	token, err := original.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if token == "" {
		t.Fatal("Encode returned empty token")
	}

	decoded, err := events.DecodeActivityFeedCursor(token)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if !decoded.OccurTime.Equal(original.OccurTime) {
		t.Errorf("OccurTime mismatch: got %v, want %v", decoded.OccurTime, original.OccurTime)
	}
	if decoded.EventID != original.EventID {
		t.Errorf("EventID mismatch: got %q, want %q", decoded.EventID, original.EventID)
	}
}

func TestDecodeActivityFeedCursor_Invalid(t *testing.T) {
	tests := []struct {
		name  string
		token string
	}{
		{"invalid base64", "!!!not-base64!!!"},
		{"invalid json", "bm90LWpzb24"},                             // base64 of "not-json"
		{"empty event_id", "eyJ0IjoiMjAyNi0wMS0wMVQwMDowMDowMFoiLCJlIjoiIn0"}, // {"t":"2026-01-01T00:00:00Z","e":""}
		{"zero time", "eyJ0IjoiMDAwMS0wMS0wMVQwMDowMDowMFoiLCJlIjoiYWJjIn0"},   // {"t":"0001-01-01T00:00:00Z","e":"abc"}
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := events.DecodeActivityFeedCursor(tt.token); err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}

func TestGetActivityHeatmap(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ch := testutil.SetupClickHouse(t)
	ctx := context.Background()

	now := time.Now().Truncate(24 * time.Hour) // Start of today (UTC)

	// Seed events across multiple days for user-1 in proj-1.
	// Day 0 (today): 3 events, Day -1: 2 events, Day -3: 1 event.
	seedEvents := []struct {
		kind  string
		day   time.Time
		count int
	}{
		{"page_view", now, 3},
		{"page_view", now.AddDate(0, 0, -1), 2},
		{"signup", now.AddDate(0, 0, -3), 1},
	}
	for _, se := range seedEvents {
		for i := 0; i < se.count; i++ {
			err := ch.Conn.Exec(ctx,
				`INSERT INTO events (event_id, project_id, distinct_id, kind, auto_properties, custom_properties, occur_time, session_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				uuid.NewString(), "proj-1", "user-1", se.kind,
				map[string]string{},
				map[string]string{},
				se.day.Add(time.Duration(i)*time.Hour),
				uuid.NewString(),
			)
			if err != nil {
				t.Fatalf("seed event: %v", err)
			}
		}
	}

	// Seed alias: anon-1 -> user-1.
	err := ch.Conn.Exec(ctx,
		`INSERT INTO profile_aliases (alias_id, profile_id, external_id, project_id) VALUES (?, ?, ?, ?)`,
		"anon-1", "user-1", "ext-1", "proj-1",
	)
	if err != nil {
		t.Fatalf("seed alias: %v", err)
	}

	// Seed event under alias on day -1.
	err = ch.Conn.Exec(ctx,
		`INSERT INTO events (event_id, project_id, distinct_id, kind, auto_properties, custom_properties, occur_time, session_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		uuid.NewString(), "proj-1", "anon-1", "anon_action",
		map[string]string{},
		map[string]string{},
		now.AddDate(0, 0, -1).Add(30*time.Minute),
		uuid.NewString(),
	)
	if err != nil {
		t.Fatalf("seed anon event: %v", err)
	}

	// Seed event in different project (should not appear in proj-1 queries).
	err = ch.Conn.Exec(ctx,
		`INSERT INTO events (event_id, project_id, distinct_id, kind, auto_properties, custom_properties, occur_time, session_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		uuid.NewString(), "proj-2", "user-1", "other_project",
		map[string]string{},
		map[string]string{},
		now,
		uuid.NewString(),
	)
	if err != nil {
		t.Fatalf("seed other project event: %v", err)
	}

	reader := events.NewReader(ch.Conn)

	fullRange := &commonv1.TimeRange{
		From: timestamppb.New(now.AddDate(0, 0, -7)),
		To:   timestamppb.New(now.AddDate(0, 0, 1)),
	}

	t.Run("aggregates per-day counts", func(t *testing.T) {
		days, err := reader.GetActivityHeatmap(ctx, events.ActivityHeatmapParams{
			ProjectID:  "proj-1",
			DistinctID: "user-1",
			TimeRange:  fullRange,
		})
		if err != nil {
			t.Fatalf("GetActivityHeatmap: %v", err)
		}
		// Day -3: 1, Day -1: 3 (2 user-1 + 1 anon-1), Day 0: 3
		if len(days) != 3 {
			t.Fatalf("expected 3 days, got %d: %+v", len(days), days)
		}
	})

	t.Run("includes alias events", func(t *testing.T) {
		days, err := reader.GetActivityHeatmap(ctx, events.ActivityHeatmapParams{
			ProjectID:  "proj-1",
			DistinctID: "user-1",
			TimeRange: &commonv1.TimeRange{
				From: timestamppb.New(now.AddDate(0, 0, -2)),
				To:   timestamppb.New(now),
			},
		})
		if err != nil {
			t.Fatalf("GetActivityHeatmap: %v", err)
		}
		// Day -1: 2 user-1 + 1 anon-1 = 3
		if len(days) != 1 {
			t.Fatalf("expected 1 day, got %d: %+v", len(days), days)
		}
		if days[0].Count != 3 {
			t.Errorf("expected count 3 (including alias), got %d", days[0].Count)
		}
	})

	t.Run("time range boundary is half-open", func(t *testing.T) {
		// [from, to) — from inclusive, to exclusive
		days, err := reader.GetActivityHeatmap(ctx, events.ActivityHeatmapParams{
			ProjectID:  "proj-1",
			DistinctID: "user-1",
			TimeRange: &commonv1.TimeRange{
				From: timestamppb.New(now),
				To:   timestamppb.New(now.AddDate(0, 0, 1)),
			},
		})
		if err != nil {
			t.Fatalf("GetActivityHeatmap: %v", err)
		}
		if len(days) != 1 {
			t.Fatalf("expected 1 day, got %d: %+v", len(days), days)
		}
		if days[0].Count != 3 {
			t.Errorf("expected 3 events on boundary day, got %d", days[0].Count)
		}
	})

	t.Run("project isolation", func(t *testing.T) {
		days, err := reader.GetActivityHeatmap(ctx, events.ActivityHeatmapParams{
			ProjectID:  "proj-2",
			DistinctID: "user-1",
			TimeRange:  fullRange,
		})
		if err != nil {
			t.Fatalf("GetActivityHeatmap: %v", err)
		}
		if len(days) != 1 {
			t.Fatalf("expected 1 day for proj-2, got %d", len(days))
		}
		if days[0].Count != 1 {
			t.Errorf("expected 1 event for proj-2, got %d", days[0].Count)
		}
	})

	t.Run("empty for nonexistent profile", func(t *testing.T) {
		days, err := reader.GetActivityHeatmap(ctx, events.ActivityHeatmapParams{
			ProjectID:  "proj-1",
			DistinctID: "nonexistent",
			TimeRange:  fullRange,
		})
		if err != nil {
			t.Fatalf("GetActivityHeatmap: %v", err)
		}
		if len(days) != 0 {
			t.Errorf("expected 0 days for nonexistent profile, got %d", len(days))
		}
	})

	t.Run("ordered by day ascending", func(t *testing.T) {
		days, err := reader.GetActivityHeatmap(ctx, events.ActivityHeatmapParams{
			ProjectID:  "proj-1",
			DistinctID: "user-1",
			TimeRange:  fullRange,
		})
		if err != nil {
			t.Fatalf("GetActivityHeatmap: %v", err)
		}
		for i := 1; i < len(days); i++ {
			if days[i].Date <= days[i-1].Date {
				t.Errorf("not ordered ASC: [%d]=%s <= [%d]=%s", i, days[i].Date, i-1, days[i-1].Date)
			}
		}
	})

	t.Run("multiple events same day bucketed correctly", func(t *testing.T) {
		days, err := reader.GetActivityHeatmap(ctx, events.ActivityHeatmapParams{
			ProjectID:  "proj-1",
			DistinctID: "user-1",
			TimeRange: &commonv1.TimeRange{
				From: timestamppb.New(now),
				To:   timestamppb.New(now.AddDate(0, 0, 1)),
			},
		})
		if err != nil {
			t.Fatalf("GetActivityHeatmap: %v", err)
		}
		if len(days) != 1 {
			t.Fatalf("expected 1 day, got %d", len(days))
		}
		if days[0].Count != 3 {
			t.Errorf("expected 3 events bucketed on same day, got %d", days[0].Count)
		}
	})
}
