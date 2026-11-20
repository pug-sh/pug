package seed

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/pug-sh/pug/internal/attribution"
)

// aiWebShare is the share of all web sessions an AI product sends: aiVisitShare
// of the three quarters that arrive without a campaign.
const aiWebShare = aiVisitShare * 0.75

// AI visits are human sessions, so only a human backfill writes their history.
// A project seeded before them gets theirs here instead, without a rebuild.

type aiVisitStore interface {
	earliestAIVisit(ctx context.Context, projectID string) (time.Time, bool, error)
	webSessionsByDay(ctx context.Context, projectID string, from time.Time) (map[string]uint64, error)
	profiledUsers(ctx context.Context, projectID string) (map[string]bool, error)
	insertEvents(ctx context.Context, projectID string, events []event) error
}

// backfillAIVisits gives each UTC day from `from` to now aiWebShare of its web
// sessions as AI visits, newest first, one day per insert, carrying on before
// the earliest AI visit stored as the crawler backfill does. Visits go only to
// users who already have a profile, so none is created here.
func backfillAIVisits(ctx context.Context, store aiVisitStore, projectID string, growthUntil, from, now time.Time) (int, error) {
	start := utcDay(from)
	dayStart, until := utcDay(now), now
	earliest, ok, err := store.earliestAIVisit(ctx, projectID)
	if err != nil {
		return 0, fmt.Errorf("find earliest AI visit: %w", err)
	}
	if ok {
		until = utcDay(earliest)
		dayStart = until.Add(-oneDay)
	}
	if dayStart.Before(start) {
		return 0, nil
	}
	webSessions, err := store.webSessionsByDay(ctx, projectID, start)
	if err != nil {
		return 0, fmt.Errorf("count web sessions: %w", err)
	}
	users, err := store.profiledUsers(ctx, projectID)
	if err != nil {
		return 0, fmt.Errorf("list profiles: %w", err)
	}
	f := newSessionFactory()
	f.growthUntil = growthUntil

	inserted := 0
	for ; !dayStart.Before(start); dayStart = dayStart.Add(-oneDay) {
		if err := ctx.Err(); err != nil {
			return inserted, err
		}
		events := f.aiVisitsOn(ctx, dayStart, until, webSessions[dayStart.Format(time.DateOnly)], users)
		if len(events) > 0 {
			if err := store.insertEvents(ctx, projectID, events); err != nil {
				return inserted, fmt.Errorf("insert AI visits of %s: %w", dayStart.Format(time.DateOnly), err)
			}
		}
		inserted += len(events)
		until = dayStart
	}
	return inserted, nil
}

// aiVisitsOn makes the AI visits of the UTC day starting at dayStart, before
// until, for a day of webSessions.
func (f *sessionFactory) aiVisitsOn(ctx context.Context, dayStart, until time.Time, webSessions uint64, users map[string]bool) []event {
	if !until.After(dayStart) {
		return nil
	}
	expected := float64(webSessions) * aiWebShare * aiGrowth(dayStart, f.growthUntil)
	n := int(expected)
	if rand.Float64() < expected-float64(n) {
		n++
	}
	var out []event
	for range n {
		u, prof, activeStart, activeEnd, ok := f.pickAIVisitor(dayStart, until, users)
		if !ok {
			continue
		}
		start := f.sampleStart(ctx, u.geo, prof.mobile, activeStart, activeEnd)
		// A campaign visit can't come from an AI product, so draw until the session is one.
		for range 16 {
			sess := buildSession(u, prof, pickJourney(prof.platform, u.member), start, until, nil, 1)
			if len(sess) > 0 && sess[0].autoProperties["$channel"] == attribution.ChannelAIAssistants {
				out = append(out, sess...)
				break
			}
		}
	}
	return out
}

// pickAIVisitor draws a user active between start and end who has a profile and
// a web device.
func (f *sessionFactory) pickAIVisitor(start, end time.Time, users map[string]bool) (*userProfile, deviceProfile, time.Time, time.Time, bool) {
	for range 64 {
		u, activeStart, activeEnd := f.pickActiveUser(start, end)
		if !users[u.id] {
			continue
		}
		for _, d := range u.devices {
			if d.platform == "web" {
				return u, d, activeStart, activeEnd, true
			}
		}
	}
	return nil, deviceProfile{}, time.Time{}, time.Time{}, false
}

// BackfillAIVisits gives projectID the AI visits its history predates, over the
// human backfill's window. growthUntil is the project's create time.
func BackfillAIVisits(ctx context.Context, ch driver.Conn, projectID string, growthUntil time.Time) error {
	started := time.Now()
	n, err := backfillAIVisits(ctx, chStore{ch: ch}, projectID, growthUntil, backfillFrom(started), started)
	if err != nil {
		return fmt.Errorf("backfill AI visits: %w", err)
	}
	slog.InfoContext(ctx, "AI visit backfill done",
		slog.String("project_id", projectID),
		slog.Int("inserted", n),
		slog.String("elapsed", time.Since(started).Round(time.Second).String()),
	)
	return nil
}

func (s chStore) earliestAIVisit(ctx context.Context, projectID string) (time.Time, bool, error) {
	var (
		earliest time.Time
		n        uint64
	)
	if err := s.ch.QueryRow(ctx,
		"SELECT min(occur_time), count() FROM events WHERE project_id = ? AND channel = ?",
		projectID, attribution.ChannelAIAssistants,
	).Scan(&earliest, &n); err != nil {
		return time.Time{}, false, err
	}
	return earliest, n > 0, nil
}

// webSessionsByDay counts human web sessions per UTC day, keyed "2006-01-02".
func (s chStore) webSessionsByDay(ctx context.Context, projectID string, from time.Time) (map[string]uint64, error) {
	rows, err := s.ch.Query(ctx, `
		SELECT toString(toDate(occur_time, 'UTC')), uniqExact(session_id)
		FROM events
		WHERE project_id = ? AND platform = 'web' AND NOT bot AND occur_time >= toDateTime64(?, 3)
		GROUP BY 1`,
		projectID, from,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]uint64{}
	for rows.Next() {
		var (
			day string
			n   uint64
		)
		if err := rows.Scan(&day, &n); err != nil {
			return nil, err
		}
		out[day] = n
	}
	return out, rows.Err()
}

func (s chStore) profiledUsers(ctx context.Context, projectID string) (map[string]bool, error) {
	rows, err := s.ch.Query(ctx, "SELECT DISTINCT id FROM profiles WHERE project_id = ?", projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}
