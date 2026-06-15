// Package demo runs the rolling demo-traffic generator: an eternally-running
// worker that plays Pug & Pals sessions out in real time so the public demo
// dashboard and live map always show fresh data.
//
// Sessions are spawned at a Poisson-ish rate shaped by the seed generator's
// diurnal/weekly curve, and each session's events are published one at a
// time — at their generated timestamps — through the same NATS subject the
// SDK ingestion path uses, so the events worker and the rollup materialized
// view ingest them exactly like real traffic.
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
	"math"
	"math/rand/v2"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/sethvargo/go-envconfig"
	"google.golang.org/protobuf/types/known/timestamppb"

	seed "github.com/pug-sh/pug/internal/app/seed/clickhouse"
	pgseed "github.com/pug-sh/pug/internal/app/seed/postgres"
	"github.com/pug-sh/pug/internal/core/events"
	clickhousedeps "github.com/pug-sh/pug/internal/deps/clickhouse"
	"github.com/pug-sh/pug/internal/deps/nats"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	eventsv1 "github.com/pug-sh/pug/internal/gen/proto/sdk/events/v1"
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

	projectID, err := ensureSeed(ctx, cfg)
	if err != nil {
		return err
	}

	natsClient, err := nats.New(ctx)
	if err != nil {
		return err
	}
	defer natsClient.Close()

	return StartWorker(ctx, natsClient, cfg, projectID)
}

// ensureSeed derives the demo project from the demo user and backfills it the
// first time the worker starts against an empty ClickHouse. It always ensures
// the Postgres customer/org/project + profiles exist (creating them on a fresh
// database, resolving them otherwise), then backfills a few months of
// historical events so the public dashboard has history behind the live feed.
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
func ensureSeed(ctx context.Context, cfg Config) (string, error) {
	var pgCfg postgres.Config
	if err := envconfig.Process(ctx, &pgCfg); err != nil {
		return "", err
	}
	pg, err := postgres.NewWriterPool(ctx, &pgCfg)
	if err != nil {
		return "", err
	}
	defer pg.Close()

	var chCfg clickhousedeps.Config
	if err := envconfig.Process(ctx, &chCfg); err != nil {
		return "", err
	}
	chDB, err := clickhousedeps.NewFromConfig(ctx, &chCfg)
	if err != nil {
		return "", err
	}
	defer func() {
		if err := chDB.Conn.Close(); err != nil {
			slog.ErrorContext(ctx, "failed to close clickhouse connection", slogx.Error(err))
		}
	}()

	project, err := pgseed.SeedProject(ctx, pg)
	if err != nil {
		return "", fmt.Errorf("seed postgres: %w", err)
	}

	n, err := seed.EventCount(ctx, chDB.Conn, project.ID)
	if err != nil {
		return "", fmt.Errorf("check demo data: %w", err)
	}
	if n >= uint64(cfg.SeedCount) {
		slog.InfoContext(ctx, "demo data present, skipping backfill",
			slog.String("project_id", project.ID),
			slog.Uint64("events", n),
		)
		return project.ID, nil
	}
	if n > 0 {
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
	}

	slog.InfoContext(ctx, "no demo data found, backfilling before live traffic",
		slog.String("project_id", project.ID),
		slog.Int64("count", cfg.SeedCount),
	)

	if err := seed.Backfill(ctx, pg, chDB.Conn, project.ID, cfg.SeedCount, cfg.SeedBatch); err != nil {
		return "", fmt.Errorf("backfill clickhouse: %w", err)
	}

	slog.InfoContext(ctx, "demo backfill complete", slog.String("project_id", project.ID))
	return project.ID, nil
}

type worker struct {
	publisher *events.Publisher
	gen       *seed.LiveGenerator
	projectID string
}

// liveBotShare sets the flat crawler session rate relative to the configured
// human peak. Bots don't follow the diurnal curve — they crawl around the
// clock, so the bot share of traffic realistically rises at night.
const liveBotShare = 0.04

func StartWorker(ctx context.Context, natsClient *nats.NATSClient, cfg Config, projectID string) error {
	w := &worker{
		publisher: events.NewPublisher(natsClient.GetJetStream()),
		gen:       seed.NewLiveGenerator(),
		projectID: projectID,
	}

	slog.InfoContext(ctx, "Starting demo traffic generator",
		slog.String("project_id", projectID),
		slog.Float64("peak_sessions_per_min", cfg.PeakSessionsPerMin),
	)

	var wg sync.WaitGroup
	defer wg.Wait()

	// Crawlers: constant low rate, 24/7.
	wg.Go(func() {
		w.spawnLoop(ctx, &wg, func() float64 { return cfg.PeakSessionsPerMin * liveBotShare }, func() {
			w.play(ctx, w.gen.LiveBotSession(time.Now()))
		})
	})

	// Humans: rate follows the diurnal/weekly/episode curve.
	w.spawnLoop(ctx, &wg, func() float64 {
		return cfg.PeakSessionsPerMin * seed.TrafficFactor(time.Now())
	}, func() {
		w.play(ctx, w.gen.LiveSession(time.Now()))
	})
	return nil
}

// spawnLoop spawns sessions as a Poisson process whose rate (sessions/min)
// is re-evaluated before each arrival. Delays are clamped so a quiet curve
// still emits something and a spike can't busy-loop.
func (w *worker) spawnLoop(ctx context.Context, wg *sync.WaitGroup, rate func() float64, spawn func()) {
	for {
		mean := time.Duration(float64(time.Minute) / rate())
		delay := time.Duration(float64(mean) * rand.ExpFloat64())
		delay = min(max(delay, time.Second), 5*time.Minute)

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}

		wg.Go(spawn)
	}
}

// play publishes a session's events as their timestamps come due, so the
// live feed sees a believable human pace.
func (w *worker) play(ctx context.Context, sess []seed.LiveEvent) {
	for _, e := range sess {
		if wait := time.Until(e.OccurTime); wait > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}

		if err := w.publisher.Publish(ctx, w.projectID, []*eventsv1.Event{toProtoEvent(e)}); err != nil {
			// Publisher already logged and recorded the error; drop the rest
			// of the session rather than retrying — this is synthetic
			// traffic and the next session is seconds away.
			return
		}
	}
}

func toProtoEvent(e seed.LiveEvent) *eventsv1.Event {
	return &eventsv1.Event{
		EventId:          &e.EventID,
		DistinctId:       &e.DistinctID,
		SessionId:        &e.SessionID,
		Kind:             &e.Kind,
		OccurTime:        timestamppb.New(e.OccurTime),
		AutoProperties:   toPropertyValues(e.AutoProperties),
		CustomProperties: toPropertyValues(e.CustomProperties),
	}
}

func toPropertyValues(props map[string]any) map[string]*commonv1.PropertyValue {
	if len(props) == 0 {
		return nil
	}
	out := make(map[string]*commonv1.PropertyValue, len(props))
	for k, v := range props {
		out[k] = toPropertyValue(v)
	}
	return out
}

// toPropertyValue maps the generator's typed Go values onto PropertyValue
// slots, matching the typing the enrichment pipeline applies to real traffic
// (bot scores/screens → Int, lat/long/amounts → Double, flags → Bool).
func toPropertyValue(v any) *commonv1.PropertyValue {
	switch x := v.(type) {
	case string:
		return &commonv1.PropertyValue{Value: &commonv1.PropertyValue_StringValue{StringValue: x}}
	case bool:
		return &commonv1.PropertyValue{Value: &commonv1.PropertyValue_BoolValue{BoolValue: x}}
	case int:
		return &commonv1.PropertyValue{Value: &commonv1.PropertyValue_IntValue{IntValue: int64(x)}}
	case int64:
		return &commonv1.PropertyValue{Value: &commonv1.PropertyValue_IntValue{IntValue: x}}
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return &commonv1.PropertyValue{Value: &commonv1.PropertyValue_StringValue{StringValue: fmt.Sprint(x)}}
		}
		return &commonv1.PropertyValue{Value: &commonv1.PropertyValue_DoubleValue{DoubleValue: x}}
	default:
		return &commonv1.PropertyValue{Value: &commonv1.PropertyValue_StringValue{StringValue: fmt.Sprint(x)}}
	}
}
