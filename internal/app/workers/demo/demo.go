// Package demo runs the rolling demo-traffic generator: an eternally-running
// worker that plays Pug & Pals sessions out in real time so the public demo
// dashboard and live map always show fresh data.
//
// Sessions are spawned at a Poisson-ish rate shaped by the seed generator's
// diurnal/weekly curve, and each session's events are written one at a time —
// at their generated timestamps — straight into ClickHouse via the same insert
// path the historical backfill uses. The worker is fully self-contained: it
// owns its ClickHouse connection and depends on no other workers (no NATS hop),
// so a single deployment seeds-then-streams. The rollup materialized view still
// fires on these direct inserts, exactly as for real traffic.
//
// The demo project is derived from the demo user (woof@pug.sh), seeded on
// first run, so no project id needs to be configured. Under `pug dev` the
// worker is opt-in via PUG_DEMO_ENABLED; the standalone command always runs.
// Peak volume is controlled by PUG_DEMO_PEAK_SESSIONS_PER_MIN (default 6 ≈
// 30-50k events/day with the default journey mix).
package demo

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/sethvargo/go-envconfig"

	seed "github.com/pug-sh/pug/internal/app/seed/clickhouse"
	pgseed "github.com/pug-sh/pug/internal/app/seed/postgres"
	clickhousedeps "github.com/pug-sh/pug/internal/deps/clickhouse"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/slogx"
)

type Config struct {
	PeakSessionsPerMin float64 `env:"PUG_DEMO_PEAK_SESSIONS_PER_MIN,default=6"`
	// One-time backfill volume used when the demo project has no events yet.
	SeedCount int64 `env:"PUG_DEMO_SEED_COUNT,default=500000"`
	SeedBatch int   `env:"PUG_DEMO_SEED_BATCH,default=10000"`
}

// Enabled reports whether `pug dev` should start the demo worker. The
// standalone `pug worker demo` command always runs. The demo project is
// derived from the demo user, so no project id is configured — this is a plain
// on/off switch.
func Enabled() bool {
	v, _ := strconv.ParseBool(os.Getenv("PUG_DEMO_ENABLED"))
	return v
}

func Run(ctx context.Context) error {
	closeOtel, err := telemetry.SetupSDK(ctx)
	if err != nil {
		return err
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := closeOtel(shutdownCtx); err != nil {
			slog.ErrorContext(shutdownCtx, "failed to shutdown telemetry", slogx.Error(err))
		}
	}()

	var cfg Config
	if err := envconfig.Process(ctx, &cfg); err != nil {
		return err
	}
	if cfg.PeakSessionsPerMin <= 0 {
		return fmt.Errorf("PUG_DEMO_PEAK_SESSIONS_PER_MIN must be > 0, got %v", cfg.PeakSessionsPerMin)
	}

	// The worker owns the ClickHouse connection for both the one-time backfill
	// and the eternal live stream, so it writes traffic directly (no NATS).
	var chCfg clickhousedeps.Config
	if err := envconfig.Process(ctx, &chCfg); err != nil {
		return err
	}
	chDB, err := clickhousedeps.NewFromConfig(ctx, &chCfg)
	if err != nil {
		return err
	}
	defer func() {
		if err := chDB.Conn.Close(); err != nil {
			slog.ErrorContext(ctx, "failed to close clickhouse connection", slogx.Error(err))
		}
	}()

	projectID, err := ensureSeed(ctx, cfg, chDB.Conn)
	if err != nil {
		return err
	}

	return StartWorker(ctx, chDB.Conn, cfg, projectID)
}

type seedAction int

const (
	seedSkip        seedAction = iota // enough events present; go straight to live traffic
	seedWarnPartial                   // interrupted backfill; leave partial history, warn
	seedBackfill                      // empty project; run the one-time backfill
)

// decideSeedAction picks the backfill action from the current event count. A
// finished backfill inserts exactly seedCount rows and live traffic only grows
// it from there, so n >= seedCount is treated as "a prior run completed". That
// is a proxy, not a proof: the count alone can't distinguish a clean backfill
// from one that was interrupted early and then had live traffic push the total
// past seedCount — both read as complete. We accept that (the alternative is a
// persisted completion marker) because the blast radius is a slightly thin demo
// dashboard, not production data. 0 < n < seedCount is an interrupted backfill
// that re-running can't repair (random event ids defeat ReplacingMergeTree
// dedup). n == 0 is a fresh project.
func decideSeedAction(n uint64, seedCount int64) seedAction {
	switch {
	case n >= uint64(seedCount):
		return seedSkip
	case n > 0:
		return seedWarnPartial
	default:
		return seedBackfill
	}
}

// ensureSeed derives the demo project from the demo user and backfills it the
// first time the worker starts against an empty ClickHouse. It ensures the
// Postgres customer/org/project exists (creating it on a fresh database,
// resolving it otherwise), backfills a few months of historical events, and
// then seeds profiles for exactly the users those events belong to — so a
// profile never exists for a user with no events. Users whose join date is
// still in the future stay profile-less until they sign up live.
//
// The backfill runs only when the project has fewer than SeedCount events. A
// finished backfill inserts exactly SeedCount rows and live traffic only grows
// the count from there, so n >= SeedCount means a prior run completed it and we
// skip straight to live. A project stuck between 1 and SeedCount-1 events is a
// backfill that was interrupted mid-run (crash, OOM, pod eviction); because the
// synthetic events carry random ids the events ReplacingMergeTree can't dedup
// across runs, re-running would duplicate rather than repair, so ensureSeed
// logs a warning and leaves the partial history in place (recovery is to
// truncate the events table and restart). Returns the demo project id to play
// live traffic into.
func ensureSeed(ctx context.Context, cfg Config, ch driver.Conn) (string, error) {
	// Postgres is only needed while seeding (account + active-set profiles), so it
	// is opened and closed here; the caller owns the long-lived ClickHouse conn.
	var pgCfg postgres.Config
	if err := envconfig.Process(ctx, &pgCfg); err != nil {
		return "", err
	}
	pg, err := postgres.NewWriterPool(ctx, &pgCfg)
	if err != nil {
		return "", err
	}
	defer pg.Close()

	project, err := pgseed.SeedAccount(ctx, pg)
	if err != nil {
		return "", fmt.Errorf("seed postgres account: %w", err)
	}

	n, err := seed.EventCount(ctx, ch, project.ID)
	if err != nil {
		return "", fmt.Errorf("check demo data: %w", err)
	}
	switch action := decideSeedAction(n, cfg.SeedCount); action {
	case seedSkip:
		// Events are present, so a prior backfill ran. Profiles are seeded in a
		// later step (SeedProfilesForUsers + CopyProfilesToClickHouse); if the
		// worker crashed between the backfill committing and profiles finishing,
		// the event-count gate alone would silently skip re-seeding and serve a
		// profile-less dashboard. Surface that loudly (mirroring the partial-
		// backfill guard). Live traffic still lazily creates profiles for users
		// it re-sees, so this self-heals for active users over time.
		profiles, err := seed.ProfileCount(ctx, ch, project.ID)
		if err != nil {
			return "", fmt.Errorf("check demo profiles: %w", err)
		}
		if profiles == 0 {
			slog.WarnContext(ctx, "demo events present but no profiles; a prior seed likely crashed mid-run",
				slog.String("project_id", project.ID),
				slog.Uint64("events", n),
				slog.String("recovery", "TRUNCATE TABLE events and TABLE profiles, then restart the demo worker to re-seed"),
			)
		} else {
			slog.InfoContext(ctx, "demo data present, skipping backfill",
				slog.String("project_id", project.ID),
				slog.Uint64("events", n),
				slog.Uint64("profiles", profiles),
			)
		}
		return project.ID, nil
	case seedWarnPartial:
		// A previous backfill was interrupted before it finished. Re-running
		// would duplicate rather than repair (each event has a random id the
		// ReplacingMergeTree can't dedup across runs), so surface it loudly
		// instead of silently shipping a thin dashboard.
		slog.WarnContext(ctx, "incomplete demo backfill detected, leaving partial history in place",
			slog.String("project_id", project.ID),
			slog.Uint64("events", n),
			slog.Int64("expected", cfg.SeedCount),
			slog.String("recovery", "TRUNCATE TABLE events and restart the demo worker to re-seed"),
		)
		return project.ID, nil
	case seedBackfill:
		// Empty project — fall through to the one-time backfill below.
	default:
		return "", fmt.Errorf("unhandled seed action %d", action)
	}

	slog.InfoContext(ctx, "no demo data found, backfilling before live traffic",
		slog.String("project_id", project.ID),
		slog.Int64("count", cfg.SeedCount),
	)

	// Backfill events first and collect the users that actually produced events,
	// then seed Postgres profiles for exactly those users and copy them to
	// ClickHouse. This ordering is what guarantees a profile only ever exists for
	// a user with events. Most of the pool stays profile-less: the ~half whose
	// join date is still in the future (they sign up live as the wall clock
	// crosses their join) plus past users who churned before the backfill window.
	indices, err := seed.BackfillEvents(ctx, ch, project.ID, cfg.SeedCount, cfg.SeedBatch)
	if err != nil {
		return "", fmt.Errorf("backfill events: %w", err)
	}

	if err := pgseed.SeedProfilesForUsers(ctx, pg, project.ID, indices); err != nil {
		return "", fmt.Errorf("seed profiles for active users: %w", err)
	}
	if err := seed.CopyProfilesToClickHouse(ctx, pg, ch, project.ID); err != nil {
		return "", fmt.Errorf("copy profiles to clickhouse: %w", err)
	}

	slog.InfoContext(ctx, "demo backfill complete",
		slog.String("project_id", project.ID),
		slog.Int("profiles", len(indices)),
	)
	return project.ID, nil
}

type worker struct {
	ch        driver.Conn
	gen       *seed.LiveGenerator
	projectID string
	ensured   sync.Map // distinctID -> struct{}: profile already created this run
	// insertProfile writes a live profile to ClickHouse; a field so tests can
	// substitute a fake. Defaults to seed.InsertLiveProfile in StartWorker.
	insertProfile func(ctx context.Context, ch driver.Conn, projectID string, p seed.LiveProfile) error
}

// liveBotShare sets the flat crawler session rate as a fraction of the
// configured human peak. ~4% keeps the bot filters and bot-score charts
// populated without crawlers dominating the live map. Bots don't follow the
// diurnal curve — they crawl around the clock — so their share of total traffic
// naturally rises at night when human traffic dips.
const liveBotShare = 0.04

func StartWorker(ctx context.Context, ch driver.Conn, cfg Config, projectID string) error {
	w := &worker{
		ch:            ch,
		gen:           seed.NewLiveGenerator(),
		projectID:     projectID,
		insertProfile: seed.InsertLiveProfile,
	}

	slog.InfoContext(ctx, "Starting demo traffic generator",
		slog.String("project_id", projectID),
		slog.Float64("peak_sessions_per_min", cfg.PeakSessionsPerMin),
	)

	// players tracks in-flight session goroutines; loops tracks the two
	// long-lived spawn loops. Keeping them separate means players only ever
	// gets Add'd from inside a running loop, so the final players.Wait() can't
	// race a concurrent Add — loops drain first (on ctx cancel), then sessions.
	var players, loops sync.WaitGroup
	defer players.Wait()

	// Crawlers: constant low rate, 24/7.
	loops.Go(func() {
		w.spawnLoop(ctx, &players, func() float64 { return cfg.PeakSessionsPerMin * liveBotShare }, func() {
			w.play(ctx, w.gen.LiveBotSession(time.Now()))
		})
	})

	// Humans: rate follows the diurnal/weekly/episode curve.
	loops.Go(func() {
		w.spawnLoop(ctx, &players, func() float64 {
			return cfg.PeakSessionsPerMin * seed.TrafficFactor(time.Now())
		}, func() {
			w.playHuman(ctx, w.gen.LiveSession(time.Now()))
		})
	})

	loops.Wait()
	return nil
}

// spawnLoop spawns sessions as a Poisson process whose rate (sessions/min)
// is re-evaluated before each arrival. Delays are clamped so a quiet curve
// still emits something and a spike can't busy-loop. Each spawned session is
// tracked on players so shutdown can wait for in-flight sessions.
func (w *worker) spawnLoop(ctx context.Context, players *sync.WaitGroup, rate func() float64, spawn func()) {
	for {
		mean := time.Duration(float64(time.Minute) / rate())
		delay := time.Duration(float64(mean) * rand.ExpFloat64())
		delay = min(max(delay, time.Second), 5*time.Minute)

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}

		players.Go(spawn)
	}
}

// playHuman ensures the session's user has a profile, then plays the session.
// Creating the profile here — the first time a user appears in this run — is how
// fresh signups keep showing up on the demo dashboard over time: as the wall
// clock crosses a user's join date the live generator forces their signup
// journey, and this attaches a profile to it. A profile is only ever created
// once there is a session of events to back it.
func (w *worker) playHuman(ctx context.Context, sess []seed.LiveEvent) {
	if len(sess) == 0 {
		return
	}
	w.ensureProfile(ctx, sess[0].DistinctID)
	w.play(ctx, sess)
}

// ensureProfile creates a ClickHouse profile for distinctID the first time it is
// seen this run, so the user appears on the (CH-backed) profiles page. Properties
// are deterministic per user, so re-creating an already-backfilled user leaves
// its properties, external_id and create_time unchanged — the profiles
// ReplacingMergeTree is versioned on insert_time, so this later write wins the
// merge and only its update_time (set to now) differs from the backfilled row.
// create_time is the user's join (their first-seen / anonymous-creation time,
// before identify). Bot ids are skipped. Best-effort: on failure the user is
// unmarked so the next session retries.
func (w *worker) ensureProfile(ctx context.Context, distinctID string) {
	if _, seen := w.ensured.LoadOrStore(distinctID, struct{}{}); seen {
		return
	}
	idx, ok := seed.HumanUserIndex(distinctID)
	if !ok {
		return // bot or non-user id: never gets a profile
	}
	du := seed.DemoUserAt(idx)
	props, externalID := pgseed.DemoProfileProperties(idx)

	if err := w.insertProfile(ctx, w.ch, w.projectID, seed.LiveProfile{
		ID:         distinctID,
		ExternalID: externalID,
		Properties: props,
		CreateTime: du.Join,
		UpdateTime: time.Now(),
	}); err != nil {
		w.ensured.Delete(distinctID) // allow the next session to retry
		if ctx.Err() != nil {
			return // shutting down: context.Canceled isn't a real insert failure
		}
		slog.ErrorContext(ctx, "demo: failed to create live profile",
			slogx.Error(err), slog.String("distinct_id", distinctID))
		telemetry.RecordError(ctx, err)
	}
}

// play writes a session's events straight into ClickHouse as their timestamps
// come due, so the live feed sees a believable human pace.
func (w *worker) play(ctx context.Context, sess []seed.LiveEvent) {
	for _, e := range sess {
		if wait := time.Until(e.OccurTime); wait > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}

		if err := seed.InsertLiveEvent(ctx, w.ch, w.projectID, e); err != nil {
			if ctx.Err() != nil {
				return // shutting down: context.Canceled isn't a real insert failure
			}
			// Drop the rest of the session rather than retrying — this is
			// synthetic traffic and the next session is seconds away.
			slog.ErrorContext(ctx, "demo: failed to insert live event",
				slogx.Error(err), slog.String("distinct_id", e.DistinctID))
			telemetry.RecordError(ctx, err)
			return
		}
	}
}
