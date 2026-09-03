package events_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pug-sh/pug/internal/core/events"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	"github.com/pug-sh/pug/internal/testutil"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// seedSession inserts n events for one session, one second apart, ending at end.
func seedSession(
	ctx context.Context, t *testing.T, ch *testutil.TestClickHouse,
	projectID, distinctID, sessionID string, n int, end time.Time, auto map[string]string,
) {
	t.Helper()
	for i := range n {
		testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), projectID, distinctID, "page_view", sessionID,
			auto, map[string]string{}, end.Add(-time.Duration(n-1-i)*time.Second))
	}
}

func sessionIDs(sessions []events.ProfileSession) []string {
	ids := make([]string, len(sessions))
	for i, s := range sessions {
		ids[i] = s.SessionID
	}
	return ids
}

func assertSortedDesc(t *testing.T, sort events.ProfileSessionSort, sessions []events.ProfileSession) {
	t.Helper()
	key := func(s events.ProfileSession) int64 {
		switch sort {
		case events.ProfileSessionSortDuration:
			return s.EndedAt.Sub(s.StartedAt).Milliseconds()
		case events.ProfileSessionSortEventCount:
			return s.EventCount
		case events.ProfileSessionSortStartedAt:
			return s.StartedAt.UnixMilli()
		default:
			return s.StartedAt.UnixMilli()
		}
	}
	for i := 1; i < len(sessions); i++ {
		if key(sessions[i-1]) < key(sessions[i]) {
			t.Errorf("%s not descending at %d: %d then %d", sort, i, key(sessions[i-1]), key(sessions[i]))
		}
	}
}

func encodeJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// The regression this RPC exists for: grouping one page of the activity feed
// client-side lost the oldest sessions and truncated the one on the boundary.
func TestGetProfileSessions_ListsEverySession(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ch := testutil.SetupClickHouse(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	web := map[string]string{"$browser": "Chrome", "$os": "Mac OS X", "$device": "MacBook", "$platform": "web"}

	sessionA, sessionB, sessionC, sessionD := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	seedSession(ctx, t, ch, "proj-1", "user-1", sessionA, 75, now, web)
	seedSession(ctx, t, ch, "proj-1", "user-1", sessionB, 93, now.Add(-6*24*time.Hour), web)
	seedSession(ctx, t, ch, "proj-1", "user-1", sessionC, 263, now.Add(-7*24*time.Hour), web)
	seedSession(ctx, t, ch, "proj-1", "user-1", sessionD, 4, now.Add(-8*24*time.Hour), web)

	// Another profile in the same project must not leak in.
	seedSession(ctx, t, ch, "proj-1", "user-2", uuid.NewString(), 3, now, web)

	reader := events.NewReader(ch.Conn)

	sessions, next, err := reader.GetProfileSessions(ctx, events.ProfileSessionsParams{
		ProjectID:  "proj-1",
		DistinctID: "user-1",
	})
	if err != nil {
		t.Fatalf("GetProfileSessions: %v", err)
	}
	if next != nil {
		t.Errorf("next cursor: got %+v, want nil (4 sessions fit one page)", next)
	}
	if got, want := sessionIDs(sessions), []string{sessionA, sessionB, sessionC, sessionD}; !slices.Equal(got, want) {
		t.Fatalf("sessions: got %v, want %v (newest first)", got, want)
	}

	// Start comes from the session's first event, not the page boundary.
	wantCounts := map[string]int64{sessionA: 75, sessionB: 93, sessionC: 263, sessionD: 4}
	for _, s := range sessions {
		if s.EventCount != wantCounts[s.SessionID] {
			t.Errorf("session %s: event count %d, want %d", s.SessionID, s.EventCount, wantCounts[s.SessionID])
		}
		wantDuration := time.Duration(wantCounts[s.SessionID]-1) * time.Second
		if got := s.EndedAt.Sub(s.StartedAt); got != wantDuration {
			t.Errorf("session %s: duration %s, want %s", s.SessionID, got, wantDuration)
		}
		if s.Browser != "Chrome" || s.OS != "Mac OS X" || s.Device != "MacBook" || s.Platform != "web" {
			t.Errorf("session %s: device context %q/%q/%q/%q, want Chrome/Mac OS X/MacBook/web",
				s.SessionID, s.Browser, s.OS, s.Device, s.Platform)
		}
		if s.Bot {
			t.Errorf("session %s: bot true, want false", s.SessionID)
		}
	}
}

// The list must cover the same identity set as the profile's session count.
func TestGetProfileSessions_ResolvesAliases(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ch := testutil.SetupClickHouse(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	if err := ch.Conn.Exec(ctx,
		`INSERT INTO profiles (id, project_id, external_id, properties, is_deleted, create_time, update_time) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"user-1", "proj-1", "ext-1", map[string]any{}, uint8(0), now, now,
	); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	if err := ch.Conn.Exec(ctx,
		`INSERT INTO profile_aliases (alias_id, profile_id, external_id, project_id) VALUES (?, ?, ?, ?)`,
		"anon-1", "user-1", "ext-1", "proj-1",
	); err != nil {
		t.Fatalf("seed alias: %v", err)
	}

	profileSession, externalSession, anonSession := uuid.NewString(), uuid.NewString(), uuid.NewString()
	seedSession(ctx, t, ch, "proj-1", "user-1", profileSession, 2, now, nil)
	seedSession(ctx, t, ch, "proj-1", "ext-1", externalSession, 2, now.Add(-time.Hour), nil)
	seedSession(ctx, t, ch, "proj-1", "anon-1", anonSession, 2, now.Add(-2*time.Hour), nil)

	reader := events.NewReader(ch.Conn)
	sessions, _, err := reader.GetProfileSessions(ctx, events.ProfileSessionsParams{
		ProjectID:  "proj-1",
		DistinctID: "user-1",
	})
	if err != nil {
		t.Fatalf("GetProfileSessions: %v", err)
	}
	if got, want := sessionIDs(sessions), []string{profileSession, externalSession, anonSession}; !slices.Equal(got, want) {
		t.Fatalf("sessions: got %v, want %v", got, want)
	}
}

// A session straddling the anonymous→identified merge is one row, not two.
func TestGetProfileSessions_SessionSpanningIdentities(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ch := testutil.SetupClickHouse(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	if err := ch.Conn.Exec(ctx,
		`INSERT INTO profiles (id, project_id, external_id, properties, is_deleted, create_time, update_time) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"user-1", "proj-1", "", map[string]any{}, uint8(0), now, now,
	); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	if err := ch.Conn.Exec(ctx,
		`INSERT INTO profile_aliases (alias_id, profile_id, external_id, project_id) VALUES (?, ?, ?, ?)`,
		"anon-1", "user-1", "", "proj-1",
	); err != nil {
		t.Fatalf("seed alias: %v", err)
	}

	shared := uuid.NewString()
	seedSession(ctx, t, ch, "proj-1", "anon-1", shared, 3, now.Add(-time.Minute), nil)
	seedSession(ctx, t, ch, "proj-1", "user-1", shared, 2, now, nil)

	reader := events.NewReader(ch.Conn)
	sessions, _, err := reader.GetProfileSessions(ctx, events.ProfileSessionsParams{
		ProjectID:  "proj-1",
		DistinctID: "user-1",
	})
	if err != nil {
		t.Fatalf("GetProfileSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions: got %d rows, want 1", len(sessions))
	}
	if sessions[0].EventCount != 5 {
		t.Errorf("event count: got %d, want 5 (both identities)", sessions[0].EventCount)
	}
}

func TestGetProfileSessions_Pagination(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ch := testutil.SetupClickHouse(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	// Start, duration and event count all differ, so every sort has a distinct order.
	type seed struct {
		id     string
		events int
		end    time.Time
	}
	seeds := []seed{
		{uuid.NewString(), 2, now},
		{uuid.NewString(), 5, now.Add(-time.Hour)},
		{uuid.NewString(), 3, now.Add(-2 * time.Hour)},
		{uuid.NewString(), 9, now.Add(-3 * time.Hour)},
		{uuid.NewString(), 4, now.Add(-4 * time.Hour)},
	}
	for _, s := range seeds {
		seedSession(ctx, t, ch, "proj-1", "user-1", s.id, s.events, s.end, nil)
	}

	reader := events.NewReader(ch.Conn)

	for _, sort := range []events.ProfileSessionSort{
		events.ProfileSessionSortStartedAt,
		events.ProfileSessionSortDuration,
		events.ProfileSessionSortEventCount,
	} {
		t.Run(string(sort), func(t *testing.T) {
			whole, _, err := reader.GetProfileSessions(ctx, events.ProfileSessionsParams{
				ProjectID: "proj-1", DistinctID: "user-1", Sort: sort,
			})
			if err != nil {
				t.Fatalf("GetProfileSessions: %v", err)
			}
			if len(whole) != len(seeds) {
				t.Fatalf("single page: got %d sessions, want %d", len(whole), len(seeds))
			}
			assertSortedDesc(t, sort, whole)

			var paged []events.ProfileSession
			var token *events.ProfileSessionCursor
			for page := 0; ; page++ {
				if page > len(seeds) {
					t.Fatal("pagination did not terminate")
				}
				got, next, err := reader.GetProfileSessions(ctx, events.ProfileSessionsParams{
					ProjectID: "proj-1", DistinctID: "user-1", Sort: sort, PageSize: 2, PageToken: token,
				})
				if err != nil {
					t.Fatalf("GetProfileSessions page %d: %v", page, err)
				}
				paged = append(paged, got...)
				if next == nil {
					break
				}
				token = next
			}
			if !slices.Equal(sessionIDs(paged), sessionIDs(whole)) {
				t.Errorf("paged order %v, want %v", sessionIDs(paged), sessionIDs(whole))
			}
		})
	}
}

func TestGetProfileSessions_PageTokenSortMismatch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ch := testutil.SetupClickHouse(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	seedSession(ctx, t, ch, "proj-1", "user-1", uuid.NewString(), 1, now, nil)
	seedSession(ctx, t, ch, "proj-1", "user-1", uuid.NewString(), 1, now.Add(-time.Hour), nil)

	reader := events.NewReader(ch.Conn)
	_, next, err := reader.GetProfileSessions(ctx, events.ProfileSessionsParams{
		ProjectID: "proj-1", DistinctID: "user-1", PageSize: 1,
	})
	if err != nil {
		t.Fatalf("GetProfileSessions: %v", err)
	}
	if next == nil {
		t.Fatal("expected a next cursor")
	}

	_, _, err = reader.GetProfileSessions(ctx, events.ProfileSessionsParams{
		ProjectID: "proj-1", DistinctID: "user-1", PageSize: 1,
		Sort: events.ProfileSessionSortDuration, PageToken: next,
	})
	if !errors.Is(err, events.ErrPageTokenSortMismatch) {
		t.Fatalf("error: got %v, want ErrPageTokenSortMismatch", err)
	}
}

// Session-level judgement: any tagged event drops the session whole, rather than
// returning it with an understated count.
func TestGetProfileSessions_BotExclusion(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ch := testutil.SetupClickHouse(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	human := map[string]string{"$browser": "Chrome"}
	crawler := map[string]string{"$browser": "Chrome", "$bot": "true", "$bot_reason": "user_agent"}

	humanSession, straddling, botSession := uuid.NewString(), uuid.NewString(), uuid.NewString()
	seedSession(ctx, t, ch, "proj-1", "user-1", humanSession, 2, now, human)
	seedSession(ctx, t, ch, "proj-1", "user-1", straddling, 2, now.Add(-time.Hour), human)
	seedSession(ctx, t, ch, "proj-1", "user-1", straddling, 2, now.Add(-time.Hour).Add(time.Minute), crawler)
	seedSession(ctx, t, ch, "proj-1", "user-1", botSession, 2, now.Add(-2*time.Hour), crawler)

	reader := events.NewReader(ch.Conn)

	excluded, _, err := reader.GetProfileSessions(ctx, events.ProfileSessionsParams{
		ProjectID: "proj-1", DistinctID: "user-1",
	})
	if err != nil {
		t.Fatalf("GetProfileSessions: %v", err)
	}
	if got, want := sessionIDs(excluded), []string{humanSession}; !slices.Equal(got, want) {
		t.Fatalf("default: got %v, want %v (straddling session dropped whole)", got, want)
	}

	included, _, err := reader.GetProfileSessions(ctx, events.ProfileSessionsParams{
		ProjectID: "proj-1", DistinctID: "user-1", IncludeBots: true,
	})
	if err != nil {
		t.Fatalf("GetProfileSessions include bots: %v", err)
	}
	if got, want := sessionIDs(included), []string{humanSession, straddling, botSession}; !slices.Equal(got, want) {
		t.Fatalf("include bots: got %v, want %v", got, want)
	}
	wantBot := map[string]bool{humanSession: false, straddling: false, botSession: true}
	for _, s := range included {
		if s.Bot != wantBot[s.SessionID] {
			t.Errorf("session %s: bot %v, want %v (true only when every event is tagged)",
				s.SessionID, s.Bot, wantBot[s.SessionID])
		}
	}
}

func TestGetProfileSessions_EmptyInputsReturnError(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ch := testutil.SetupClickHouse(t)
	ctx := context.Background()
	reader := events.NewReader(ch.Conn)

	for _, tc := range []struct {
		name       string
		projectID  string
		distinctID string
	}{
		{"empty project", "", "user-1"},
		{"empty distinct id", "proj-1", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := reader.GetProfileSessions(ctx, events.ProfileSessionsParams{
				ProjectID: tc.projectID, DistinctID: tc.distinctID,
			}); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestProfileSessionCursor_RoundTrip(t *testing.T) {
	c := &events.ProfileSessionCursor{
		Sort:      events.ProfileSessionSortEventCount,
		Value:     263,
		SessionID: "01a0425b-0000-0000-0000-000000000000",
	}
	token, err := c.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := events.DecodeProfileSessionCursor(token)
	if err != nil {
		t.Fatalf("DecodeProfileSessionCursor: %v", err)
	}
	if *got != *c {
		t.Errorf("round trip: got %+v, want %+v", *got, *c)
	}
}

func TestDecodeProfileSessionCursor_Invalid(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token string
	}{
		{"not base64", "!!!not-base64!!!"},
		{"not json", "bm90LWpzb24"},
		{"missing session id", encodeJSON(t, map[string]any{"s": "started_at", "v": 1})},
		{"unknown sort", encodeJSON(t, map[string]any{"s": "cost", "v": 1, "i": "abc"})},
		{"absent sort", encodeJSON(t, map[string]any{"v": 1, "i": "abc"})},
		// A zero-filled value seeks past every row and reads as end-of-list.
		{"absent value", encodeJSON(t, map[string]any{"s": "started_at", "i": "abc"})},
		{"zero started_at", encodeJSON(t, map[string]any{"s": "started_at", "v": 0, "i": "abc"})},
		{"zero event count", encodeJSON(t, map[string]any{"s": "event_count", "v": 0, "i": "abc"})},
		{"negative duration", encodeJSON(t, map[string]any{"s": "duration", "v": -1, "i": "abc"})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := events.DecodeProfileSessionCursor(tc.token); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

// A single-event session has duration 0, so that is the one legitimate zero value.
func TestDecodeProfileSessionCursor_ZeroDurationAccepted(t *testing.T) {
	token := encodeJSON(t, map[string]any{"s": "duration", "v": 0, "i": "abc"})
	if _, err := events.DecodeProfileSessionCursor(token); err != nil {
		t.Errorf("DecodeProfileSessionCursor: %v, want nil", err)
	}
}

// Ties on the sort column are the common case — every single-event session ties on
// both duration and event count. The cursor's (value, session_id) tuple is what keeps
// a page boundary inside a run of them from dropping or repeating rows.
func TestGetProfileSessions_PaginatesThroughTies(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ch := testutil.SetupClickHouse(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	// Identical event count and identical duration across all four.
	want := make([]string, 0, 4)
	for i := range 4 {
		id := uuid.NewString()
		want = append(want, id)
		seedSession(ctx, t, ch, "proj-1", "user-1", id, 3, now.Add(-time.Duration(i)*time.Hour), nil)
	}
	slices.Sort(want)

	reader := events.NewReader(ch.Conn)

	for _, sort := range []events.ProfileSessionSort{
		events.ProfileSessionSortDuration,
		events.ProfileSessionSortEventCount,
	} {
		t.Run(string(sort), func(t *testing.T) {
			var got []string
			var token *events.ProfileSessionCursor
			for page := 0; ; page++ {
				if page > 4 {
					t.Fatal("pagination did not terminate")
				}
				sessions, next, err := reader.GetProfileSessions(ctx, events.ProfileSessionsParams{
					ProjectID: "proj-1", DistinctID: "user-1", Sort: sort, PageSize: 2, PageToken: token,
				})
				if err != nil {
					t.Fatalf("GetProfileSessions page %d: %v", page, err)
				}
				got = append(got, sessionIDs(sessions)...)
				if next == nil {
					break
				}
				token = next
			}
			slices.Sort(got)
			if !slices.Equal(got, want) {
				t.Errorf("paged through ties: got %v, want each of %v exactly once", got, want)
			}
		})
	}
}

// A final page that exactly fills page_size must not hand back a cursor to an empty page.
func TestGetProfileSessions_ExactlyFullFinalPage(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ch := testutil.SetupClickHouse(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	for i := range 4 {
		seedSession(ctx, t, ch, "proj-1", "user-1", uuid.NewString(), 1, now.Add(-time.Duration(i)*time.Hour), nil)
	}

	reader := events.NewReader(ch.Conn)
	_, next, err := reader.GetProfileSessions(ctx, events.ProfileSessionsParams{
		ProjectID: "proj-1", DistinctID: "user-1", PageSize: 2,
	})
	if err != nil {
		t.Fatalf("GetProfileSessions: %v", err)
	}
	if next == nil {
		t.Fatal("page 1: expected a next cursor")
	}

	sessions, next, err := reader.GetProfileSessions(ctx, events.ProfileSessionsParams{
		ProjectID: "proj-1", DistinctID: "user-1", PageSize: 2, PageToken: next,
	})
	if err != nil {
		t.Fatalf("GetProfileSessions page 2: %v", err)
	}
	if len(sessions) != 2 {
		t.Errorf("page 2: got %d sessions, want 2", len(sessions))
	}
	if next != nil {
		t.Errorf("page 2 cursor: got %+v, want nil (4 sessions exactly fill two pages)", next)
	}
}

// Device context is argMax(occur_time), so it must come from the session's last
// event rather than its first or an arbitrary one.
func TestGetProfileSessions_DeviceContextFromLatestEvent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ch := testutil.SetupClickHouse(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	session := uuid.NewString()
	seedSession(ctx, t, ch, "proj-1", "user-1", session, 2, now.Add(-time.Hour),
		map[string]string{"$browser": "Safari", "$os": "iOS", "$device": "iPhone", "$platform": "ios"})
	seedSession(ctx, t, ch, "proj-1", "user-1", session, 2, now,
		map[string]string{"$browser": "Chrome", "$os": "Mac OS X", "$device": "MacBook", "$platform": "web"})

	reader := events.NewReader(ch.Conn)
	sessions, _, err := reader.GetProfileSessions(ctx, events.ProfileSessionsParams{
		ProjectID: "proj-1", DistinctID: "user-1",
	})
	if err != nil {
		t.Fatalf("GetProfileSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions: got %d rows, want 1", len(sessions))
	}
	got := sessions[0]
	if got.Browser != "Chrome" || got.OS != "Mac OS X" || got.Device != "MacBook" || got.Platform != "web" {
		t.Errorf("device context %q/%q/%q/%q, want Chrome/Mac OS X/MacBook/web (the latest event)",
			got.Browser, got.OS, got.Device, got.Platform)
	}
}

func TestGetProfileSessions_TimeRange(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ch := testutil.SetupClickHouse(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	recent, old := uuid.NewString(), uuid.NewString()
	seedSession(ctx, t, ch, "proj-1", "user-1", recent, 2, now, nil)
	seedSession(ctx, t, ch, "proj-1", "user-1", old, 2, now.Add(-30*24*time.Hour), nil)

	reader := events.NewReader(ch.Conn)
	sessions, _, err := reader.GetProfileSessions(ctx, events.ProfileSessionsParams{
		ProjectID: "proj-1", DistinctID: "user-1",
		TimeRange: &commonv1.TimeRange{
			From: timestamppb.New(now.Add(-24 * time.Hour)),
			To:   timestamppb.New(now.Add(time.Hour)),
		},
	})
	if err != nil {
		t.Fatalf("GetProfileSessions: %v", err)
	}
	if got, want := sessionIDs(sessions), []string{recent}; !slices.Equal(got, want) {
		t.Fatalf("sessions: got %v, want %v (the older session is outside the window)", got, want)
	}

	if _, _, err := reader.GetProfileSessions(ctx, events.ProfileSessionsParams{
		ProjectID: "proj-1", DistinctID: "user-1",
		TimeRange: &commonv1.TimeRange{From: timestamppb.New(now.Add(-24 * time.Hour))},
	}); err == nil {
		t.Error("partial TimeRange: expected an error")
	}
}
