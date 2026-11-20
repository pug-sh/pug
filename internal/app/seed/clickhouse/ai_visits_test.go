package seed

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pug-sh/pug/internal/attribution"
	chq "github.com/pug-sh/pug/internal/core/clickhouse"
	"github.com/pug-sh/pug/internal/testutil"
)

type memAIVisitStore struct {
	events      []event
	inserts     map[string]int
	webSessions uint64
	users       map[string]bool
}

func newMemAIVisitStore(users map[string]bool, webSessions uint64) *memAIVisitStore {
	return &memAIVisitStore{inserts: map[string]int{}, webSessions: webSessions, users: users}
}

func (s *memAIVisitStore) earliestAIVisit(context.Context, string) (time.Time, bool, error) {
	var earliest time.Time
	found := false
	for _, e := range s.events {
		if e.autoProperties["$channel"] == attribution.ChannelAIAssistants && (!found || e.occurTime.Before(earliest)) {
			earliest, found = e.occurTime, true
		}
	}
	return earliest, found, nil
}

func (s *memAIVisitStore) webSessionsByDay(_ context.Context, _ string, from time.Time) (map[string]uint64, error) {
	out := map[string]uint64{}
	for d := utcDay(from); d.Before(testNow.Add(3 * time.Hour)); d = d.Add(oneDay) {
		out[d.Format(time.DateOnly)] = s.webSessions
	}
	return out, nil
}

func (s *memAIVisitStore) profiledUsers(context.Context, string) (map[string]bool, error) {
	return s.users, nil
}

func (s *memAIVisitStore) insertEvents(_ context.Context, _ string, events []event) error {
	s.inserts[utcDay(events[0].occurTime).Format(time.DateOnly)]++
	s.events = append(s.events, events...)
	return nil
}

type failingAIStore struct {
	aiVisitStore
	failAfter, inserts int
}

func (s *failingAIStore) insertEvents(ctx context.Context, projectID string, events []event) error {
	if s.inserts == s.failAfter {
		return errStoreDown
	}
	s.inserts++
	return s.aiVisitStore.insertEvents(ctx, projectID, events)
}

func everyNthUser(n int) map[string]bool {
	users := map[string]bool{}
	for i := 0; i < DistinctIDPool; i += n {
		users[fmt.Sprintf("user-%05d", i)] = true
	}
	return users
}

func TestAIVisitBackfillResumes(t *testing.T) {
	from := testNow.AddDate(0, 0, -6)
	store := newMemAIVisitStore(everyNthUser(1), 800)
	if _, err := backfillAIVisits(t.Context(), &failingAIStore{aiVisitStore: store, failAfter: 3}, "proj", time.Time{}, from, testNow); err == nil {
		t.Fatal("cut run: no error")
	}
	if _, err := backfillAIVisits(t.Context(), store, "proj", time.Time{}, from, testNow.Add(time.Hour)); err != nil {
		t.Fatalf("resumed run: %v", err)
	}
	for d := utcDay(from); !d.After(testNow); d = d.Add(oneDay) {
		if n := store.inserts[d.Format(time.DateOnly)]; n != 1 {
			t.Errorf("%s written %d times, want once", d.Format(time.DateOnly), n)
		}
	}
	if n, err := backfillAIVisits(t.Context(), store, "proj", time.Time{}, from, testNow.Add(2*time.Hour)); err != nil || n != 0 {
		t.Errorf("a finished backfill ran again: inserted %d, err %v", n, err)
	}
}

func TestAIVisitsOnADay(t *testing.T) {
	f := newSessionFactory()
	users := everyNthUser(3)
	dayStart := utcDay(testNow).Add(-oneDay)
	sessions := map[string]bool{}
	for _, e := range f.aiVisitsOn(t.Context(), dayStart, dayStart.Add(oneDay), 2000, users) {
		sessions[e.sessionID] = true
		if !users[e.distinctID] {
			t.Fatalf("AI visit by %s, who has no profile", e.distinctID)
		}
		if e.autoProperties["$platform"] != "web" || e.autoProperties["$channel"] != attribution.ChannelAIAssistants {
			t.Fatalf("AI visit event on %v from %v", e.autoProperties["$platform"], e.autoProperties["$channel"])
		}
		if e.occurTime.Before(dayStart) || e.occurTime.After(dayStart.Add(oneDay)) {
			t.Fatalf("AI visit event at %v, outside its day", e.occurTime)
		}
	}
	// 3% of 2,000 web sessions.
	if n := len(sessions); n < 40 || n > 80 {
		t.Errorf("%d AI visits on a day of 2,000 web sessions, want about 60", n)
	}
}

func TestAIVisitBackfillOnClickHouse(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ch := testutil.SetupClickHouse(t)
	ctx := t.Context()
	now := time.Now().UTC()
	// A whole first day: a sliver of one can get no AI visit, which the rerun
	// below would then fill.
	from := utcDay(now).AddDate(0, 0, -2)
	store := chStore{ch: ch.Conn}

	// Two days of history from before AI visits, and a profile for each user in it.
	f := newSessionFactory()
	var history []event
	users := map[string]bool{}
	for len(history) < 12000 {
		sess := f.session(ctx, from, now)
		if len(sess) == 0 || sess[0].autoProperties["$channel"] == attribution.ChannelAIAssistants {
			continue
		}
		history = append(history, sess...)
		if _, ok := HumanUserIndex(sess[0].distinctID); ok {
			users[sess[0].distinctID] = true
		}
	}
	if err := store.insertEvents(ctx, "proj", history); err != nil {
		t.Fatal(err)
	}
	batch, err := ch.Conn.PrepareBatch(ctx, chq.ProfilesInsertStmt)
	if err != nil {
		t.Fatal(err)
	}
	for id := range users {
		if err := batch.Append(id, "proj", "", "{}", uint8(0), from, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := batch.Send(); err != nil {
		t.Fatal(err)
	}

	n, err := backfillAIVisits(ctx, store, "proj", time.Time{}, from, now)
	if err != nil || n == 0 {
		t.Fatalf("backfill inserted %d, err %v", n, err)
	}
	var visits, orphans uint64
	if err := ch.Conn.QueryRow(ctx, `
		SELECT uniqExact(session_id), countIf(distinct_id NOT IN (SELECT id FROM profiles WHERE project_id = ?))
		FROM events WHERE project_id = ? AND channel = ?`,
		"proj", "proj", attribution.ChannelAIAssistants,
	).Scan(&visits, &orphans); err != nil {
		t.Fatal(err)
	}
	if visits == 0 || orphans != 0 {
		t.Errorf("%d AI visits stored, %d events by users without a profile", visits, orphans)
	}
	if again, err := backfillAIVisits(ctx, store, "proj", time.Time{}, from, now); err != nil || again != 0 {
		t.Errorf("a finished backfill ran again: inserted %d, err %v", again, err)
	}
}
