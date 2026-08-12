// Package usage runs one event metering pass: it counts distinct events per
// project per day in ClickHouse and stores the totals in Postgres, then returns.
// Scheduling is the deployment's job (a k8s CronJob), not this process's.
//
// Metering is optional: without it GetUsage answers with an absent
// usage_computed_at rather than a wrong count, and nothing else degrades.
package usage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pug-sh/pug/internal/app/cron"
	coreusage "github.com/pug-sh/pug/internal/core/usage"
	chdb "github.com/pug-sh/pug/internal/deps/clickhouse"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/slogx"
	"github.com/sethvargo/go-envconfig"
	"go.opentelemetry.io/otel"
)

const defaultRescanDays = 2

type Config struct {
	// Trailing window the meter recomputes each run, absorbing late arrivals.
	RescanDays int `env:"PUG_USAGE_RESCAN_DAYS,default=2"`
}

// Sub-tasks held to a daily cadence regardless of how often the schedule fires.
const (
	taskFullRecompute = "full_recompute"
	taskPrune         = "prune"
)

const (
	// A year of history plus a month of slack.
	retention = 390 * 24 * time.Hour

	// Both are measured against cron_state. Pruning stays daily because no index
	// leads with `day`, so the delete scans the table.
	fullRecomputeInterval = 24 * time.Hour
	pruneInterval         = 24 * time.Hour
)

// Run meters once and returns. The error is the CronJob's exit code, so a failed
// pass must not come back nil. Lock contention is not a failure and returns nil.
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

	var pgCfg postgres.Config
	if err := envconfig.Process(ctx, &pgCfg); err != nil {
		return err
	}
	pgRO, err := postgres.NewReaderPool(ctx, &pgCfg)
	if err != nil {
		return err
	}
	defer pgRO.Close()

	pgW, err := postgres.NewWriterPool(ctx, &pgCfg)
	if err != nil {
		return err
	}
	defer pgW.Close()

	var chCfg chdb.Config
	if err := envconfig.Process(ctx, &chCfg); err != nil {
		return err
	}
	ch, err := chdb.NewReaderPool(ctx, &chCfg)
	if err != nil {
		return err
	}
	defer func() {
		if err := ch.Close(); err != nil {
			slog.WarnContext(ctx, "failed to close ClickHouse connection", slogx.Error(err))
		}
	}()

	if cfg.RescanDays <= 0 {
		slog.WarnContext(ctx, "ignoring a non-positive PUG_USAGE_RESCAN_DAYS",
			slog.Int("configured", cfg.RescanDays), slog.Int("using", defaultRescanDays))
		cfg.RescanDays = defaultRescanDays
	}
	j := &job{
		service:    coreusage.NewService(pgRO, pgW).WithClickHouse(ch),
		state:      cron.NewState(pgRO, pgW, "usage"),
		pgW:        pgW,
		rescanDays: cfg.RescanDays,
	}

	// Without a root span every telemetry.RecordError below resolves to the noop
	// span and is silently discarded.
	ctx, span := otel.Tracer("cron/usage").Start(ctx, "usage.pass")
	defer span.End()

	slog.InfoContext(ctx, "Running a usage metering pass", slog.Int("rescan_days", j.rescanDays))
	// Logged here rather than in main, which runs after closeOtel has swapped the
	// handler back to stderr.
	if err := j.run(ctx); err != nil {
		slog.ErrorContext(ctx, "usage metering pass failed", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return err
	}
	return nil
}

type job struct {
	service    *coreusage.Service
	state      *cron.State
	pgW        *pgxpool.Pool
	rescanDays int
}

func (j *job) run(ctx context.Context) error {
	return cron.WithLock(ctx, j.pgW, cron.LockUsage, func(ctx context.Context) error {
		now := time.Now().UTC()
		return errors.Join(j.meter(ctx, now), j.prune(ctx, now))
	})
}

// meter recomputes a trailing window, then re-sums every org's current period.
// Once a day it widens to the whole current month.
func (j *job) meter(ctx context.Context, now time.Time) error {
	windows, err := j.service.OrgPeriods(ctx, now)
	if err != nil {
		return err
	}

	lastFull, err := j.state.LastRun(ctx, taskFullRecompute)
	if err != nil {
		return err
	}

	from := coreusage.FloorDayUTC(now.AddDate(0, 0, -j.rescanDays))
	full := now.Sub(lastFull) >= fullRecomputeInterval
	if full {
		if periodStart, _ := coreusage.CalendarMonth(now); periodStart.Before(from) {
			from = periodStart
		}
	}

	usage, err := j.service.MeterWindow(ctx, from, now)
	if err != nil {
		return err
	}
	if err := j.service.RecordDailyUsage(ctx, usage); err != nil {
		return err
	}
	// No cells at all is far likelier a misconfigured ClickHouse than a genuinely
	// idle deployment, and reconciling on it would wipe the window's stored days.
	if len(usage) == 0 {
		slog.WarnContext(ctx, "usage meter read no cells; leaving stored days alone",
			slog.Time("from", from), slog.Time("to", now))
	} else if _, err := j.service.DeleteUnmeteredDays(ctx, usage, from, now); err != nil {
		return err
	}
	// Stamping a read we would not reconcile on defers the next wide window 24h.
	if full && len(usage) > 0 {
		if err := j.state.MarkRun(ctx, taskFullRecompute, now); err != nil {
			return err
		}
	}

	var refreshed, failed int
	var firstErr error
	for _, win := range windows {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, err := j.service.RefreshPeriodUsage(ctx, win.OrgID, win.Start, win.End); err != nil {
			failed++
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		refreshed++
	}
	if failed > 0 {
		err := fmt.Errorf("usage meter left %d of %d orgs unrefreshed: %w", failed, len(windows), firstErr)
		slog.ErrorContext(ctx, "failed to refresh period usage for some orgs", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return err
	}

	slog.InfoContext(ctx, "metered event usage",
		slog.Time("from", from), slog.Int("cells", len(usage)),
		slog.Int("orgs_refreshed", refreshed), slog.Bool("full_period", full))
	return nil
}

func (j *job) prune(ctx context.Context, now time.Time) error {
	lastPrune, err := j.state.LastRun(ctx, taskPrune)
	if err != nil {
		return err
	}
	if now.Sub(lastPrune) < pruneInterval {
		return nil
	}

	pruned, err := j.service.PruneUsage(ctx, now.Add(-retention))
	if err != nil {
		return err
	}
	if err := j.state.MarkRun(ctx, taskPrune, now); err != nil {
		return err
	}
	if pruned > 0 {
		slog.InfoContext(ctx, "pruned usage rows", slog.Int64("count", pruned))
	}
	return nil
}
