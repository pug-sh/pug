package seed

import (
	"math/rand/v2"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Live mode (rolling demo traffic)
// ---------------------------------------------------------------------------

// LiveEvent is the exported shape of a generated event, consumed by the demo
// worker which plays sessions out in real time through the ingestion
// pipeline.
type LiveEvent struct {
	EventID          string
	DistinctID       string
	SessionID        string
	Kind             string
	OccurTime        time.Time
	AutoProperties   map[string]any
	CustomProperties map[string]any
}

// LiveGenerator produces sessions anchored at the present whose event times
// extend into the near future. Unlike batch seeding it never tracks session
// windows, and per-user memory is bounded, so memory stays flat for an
// eternally-running worker. Safe for concurrent use: generation (fast) is
// serialized behind a mutex while playback (slow) happens in the caller.
type LiveGenerator struct {
	mu sync.Mutex
	f  *sessionFactory
}

func NewLiveGenerator() *LiveGenerator {
	return &LiveGenerator{f: newSessionFactory()}
}

// LiveSession builds one coherent human session starting at now. Event
// OccurTimes extend up to ~20 minutes into the future; the caller is
// expected to wait until each event's time before emitting it. Bot traffic
// is separate (LiveBotSession) so callers can schedule it on a flat 24/7
// rate instead of the human diurnal curve.
func (g *LiveGenerator) LiveSession(now time.Time) []LiveEvent {
	g.mu.Lock()
	defer g.mu.Unlock()

	// A user must be currently alive (joined, not churned); brand-new users
	// get their signup journey as they cross their join time.
	u, _, _ := g.f.pickActiveUser(now, now.Add(time.Minute))
	prof := u.devices[rand.IntN(len(u.devices))]
	jd := g.f.journeyFor(u, prof, now)
	return toLiveEvents(buildSession(u, prof, jd, now, now.Add(time.Hour), g.f.memory(u)))
}

// LiveBotSession builds one crawler session starting at now. Bots don't
// sleep: the worker schedules these at a constant rate around the clock,
// which also makes the bot share of traffic realistically rise at night.
func (g *LiveGenerator) LiveBotSession(now time.Time) []LiveEvent {
	g.mu.Lock()
	defer g.mu.Unlock()
	return toLiveEvents(g.f.botSession(now, now.Add(time.Hour)))
}

func toLiveEvents(sess []event) []LiveEvent {
	out := make([]LiveEvent, len(sess))
	for i, e := range sess {
		out[i] = LiveEvent{
			EventID:          e.eventID,
			DistinctID:       e.distinctID,
			SessionID:        e.sessionID,
			Kind:             e.kind,
			OccurTime:        e.occurTime,
			AutoProperties:   e.autoProperties,
			CustomProperties: e.customProperties,
		}
	}
	return out
}

// TrafficFactor returns the relative traffic intensity at t, following the
// same diurnal and weekly shape the batch seeder uses (on a UTC clock, as a
// store-wide aggregate) plus episode multipliers — so live traffic spikes
// during promo weeks too. Range is (0, ~1.45]: 1.0 is the normal busiest
// hour, above 1.0 means an episode is running. The demo worker multiplies
// its peak session rate by this factor.
func TrafficFactor(t time.Time) float64 {
	utc := t.UTC()
	const maxW = 1.5 * 1.45 // hour peak * day peak
	return hourWeights[utc.Hour()] * dayWeights[int(utc.Weekday())] * episodeTrafficMult(t) / maxW
}
