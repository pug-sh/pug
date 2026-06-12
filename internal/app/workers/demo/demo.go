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
// The worker is opt-in: it requires PUG_DEMO_PROJECT_ID. Peak volume is
// controlled by PUG_DEMO_PEAK_SESSIONS_PER_MIN (default 6 ≈ 30-50k
// events/day with the default journey mix).
package demo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"os"
	"sync"
	"time"

	"github.com/sethvargo/go-envconfig"
	"google.golang.org/protobuf/types/known/timestamppb"

	seed "github.com/pug-sh/pug/internal/app/seed/clickhouse"
	"github.com/pug-sh/pug/internal/core/events"
	"github.com/pug-sh/pug/internal/deps/nats"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	eventsv1 "github.com/pug-sh/pug/internal/gen/proto/sdk/events/v1"
	"github.com/pug-sh/pug/internal/slogx"
)

type Config struct {
	ProjectID          string  `env:"PUG_DEMO_PROJECT_ID"`
	PeakSessionsPerMin float64 `env:"PUG_DEMO_PEAK_SESSIONS_PER_MIN,default=6"`
}

// Enabled reports whether the demo worker is configured to run. Used by
// `pug dev` to decide whether to start it.
func Enabled() bool {
	return os.Getenv("PUG_DEMO_PROJECT_ID") != ""
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
	if cfg.ProjectID == "" {
		return errors.New("demo worker requires PUG_DEMO_PROJECT_ID")
	}
	if cfg.PeakSessionsPerMin <= 0 {
		return fmt.Errorf("PUG_DEMO_PEAK_SESSIONS_PER_MIN must be > 0, got %v", cfg.PeakSessionsPerMin)
	}

	natsClient, err := nats.New(ctx)
	if err != nil {
		return err
	}
	defer natsClient.Close()

	return StartWorker(ctx, natsClient, cfg)
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

func StartWorker(ctx context.Context, natsClient *nats.NATSClient, cfg Config) error {
	w := &worker{
		publisher: events.NewPublisher(natsClient.GetJetStream()),
		gen:       seed.NewLiveGenerator(),
		projectID: cfg.ProjectID,
	}

	slog.InfoContext(ctx, "Starting demo traffic generator",
		slog.String("project_id", cfg.ProjectID),
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
