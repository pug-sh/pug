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
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const (
	defaultRescanDays = 2

	// Past the retention window a wider rescan re-inserts day cells that the same
	// pass's prune then deletes, every pass, forever — and the ClickHouse scan stops
	// pruning partitions long before that. Expressed from retention so the two
	// cannot drift.
	maxRescanDays = int(retention / (24 * time.Hour))
)

type Config struct {
	// Trailing window the meter recomputes each run, absorbing late arrivals.
	// No envconfig default: an unset var and an explicit 0 both resolve through
	// rescanDays, so defaultRescanDays stays the single source for the number.
	RescanDays int `env:"PUG_USAGE_RESCAN_DAYS"`
}

// rescanDays clamps a configured window to something the meter can act on. A
// negative value would put `from` in the future, so every read comes back empty
// and the pass meters nothing -- forever, and quietly.
func rescanDays(ctx context.Context, configured int) int {
	switch {
	case configured > maxRescanDays:
		slog.WarnContext(ctx, "clamping PUG_USAGE_RESCAN_DAYS to the retention window",
			slog.Int("configured", configured), slog.Int("using", maxRescanDays))
		return maxRescanDays
	case configured > 0:
		return configured
	case configured < 0:
		slog.WarnContext(ctx, "ignoring a negative PUG_USAGE_RESCAN_DAYS",
			slog.Int("configured", configured), slog.Int("using", defaultRescanDays))
	}
	return defaultRescanDays
}

// Sub-tasks held to a daily cadence regardless of how often the schedule fires.
const (
	taskFullRecompute cron.Task = "full_recompute"
	taskPrune         cron.Task = "prune"
)

const (
	// A year of history plus 25 days of slack.
	retention = 390 * 24 * time.Hour

	// Both are measured against cron_state. Pruning stays daily because the
	// retention boundary only moves once a day, so a more frequent pass would
	// re-scan the same range to delete nothing.
	fullRecomputeInterval = 24 * time.Hour
	pruneInterval         = 24 * time.Hour
)

// passTimeout bounds one pass end to end. The pass holds an advisory lock for its
// whole duration, so a hang does not merely stall this run: every later pod finds
// the lock held, logs "another pass holds the cron lock", and exits 0, leaving a
// green CronJob and frozen stamps for as long as the wedged pod lives.
// docs/architecture/usage.md defers the bound to the CronJob's
// activeDeadlineSeconds, but that lives in a manifest this repo does not ship, so
// it has to exist here too. Generous on purpose: a full-month uniqExact over a
// large tenant is minutes.
const passTimeout = 30 * time.Minute

const (
	outcomeMetered     = "metered"
	outcomeUnverified  = "unverified"
	outcomeLockHeld    = "lock_held"
	outcomeSetupFailed = "setup_failed"
	outcomeFailed      = "failed"

	reasonUnverifiedRead  = "unverified_read"
	reasonUnknownProjects = "unknown_projects"
)

var (
	passCounter        metric.Int64Counter
	passDuration       metric.Float64Histogram
	unrefreshedCounter metric.Int64Counter
	droppedDayCounter  metric.Int64Counter
)

// Machine-readable companions to the pass's log lines. Registration errors are
// dropped rather than fatal: the alert that matters is usage_computed_at going
// stale (docs/architecture/usage.md section 7), which these only explain.
func init() {
	meter := otel.Meter("github.com/pug-sh/pug/internal/app/cron/usage")
	passCounter, _ = meter.Int64Counter(
		"usage.pass_total",
		metric.WithDescription("Metering passes by outcome. metered and lock_held exit 0; unverified refreshed nothing and exits 0; setup_failed and failed exit non-zero. A telemetry setup failure emits no sample at all."),
	)
	passDuration, _ = meter.Float64Histogram(
		"usage.pass_duration_seconds",
		metric.WithUnit("s"),
		metric.WithDescription("Wall time of one pass, tagged by outcome. Approaching passTimeout means every later pass finds the lock held and exits 0."),
	)
	unrefreshedCounter, _ = meter.Int64Counter(
		"usage.unrefreshed_total",
		metric.WithDescription("Passes that refreshed no period because the read could not be trusted. Any sustained non-zero rate means stamps are going stale behind a green CronJob."),
	)
	droppedDayCounter, _ = meter.Int64Counter(
		"usage.dropped_days_total",
		metric.WithDescription("Stored day cells reconciled away because ClickHouse no longer returns them. Expected on erasure or a dropped partition; also what a truncated read looks like."),
	)
}

// setupFailed reports a dependency that would not come up. These all return
// before the pass starts, so without an explicit log+record they reach only
// main's stderr line — printed after closeOtel has swapped the handler back — and
// never OTLP, which is where a misconfigured deployment is diagnosed.
func setupFailed(ctx context.Context, dependency string, err error) error {
	err = fmt.Errorf("usage meter setup: %s: %w", dependency, err)
	slog.ErrorContext(ctx, "usage metering pass could not start", slogx.Error(err),
		slog.String("dependency", dependency))
	telemetry.RecordError(ctx, err)
	return err
}

// Run meters once and returns. The error is the CronJob's exit code, so a failed
// pass must not come back nil. Lock contention is not a failure and returns nil.
func Run(ctx context.Context) error {
	// The one failure with nowhere to go: telemetry is not up, so this returns
	// bare and surfaces through main's stderr line.
	closeOtel, err := telemetry.SetupSDK(ctx)
	if err != nil {
		return err
	}
	defer telemetry.ShutdownOnExit(ctx, closeOtel)

	// Started before config and pools rather than after: telemetry.RecordError
	// resolves to the noop span until a root span exists, so anything recorded
	// during setup would be silently discarded — and setup is exactly where a
	// misconfigured deployment fails.
	ctx, span := otel.Tracer("cron/usage").Start(ctx, "usage.pass")
	defer span.End()

	ctx, cancel := context.WithTimeout(ctx, passTimeout)
	defer cancel()

	// Registered after cancel so LIFO runs it first, on a context still live.
	// Setup is the default: every return before the meter starts is one.
	start := time.Now()
	outcome := outcomeSetupFailed
	defer func() {
		attrs := metric.WithAttributes(attribute.String("outcome", outcome))
		passCounter.Add(ctx, 1, attrs)
		passDuration.Record(ctx, time.Since(start).Seconds(), attrs)
	}()

	var cfg Config
	if err := envconfig.Process(ctx, &cfg); err != nil {
		return setupFailed(ctx, "usage config", err)
	}

	var pgCfg postgres.Config
	if err := envconfig.Process(ctx, &pgCfg); err != nil {
		return setupFailed(ctx, "postgres config", err)
	}
	pgRO, err := postgres.NewReaderPool(ctx, &pgCfg)
	if err != nil {
		return setupFailed(ctx, "postgres reader pool", err)
	}
	defer pgRO.Close()

	pgW, err := postgres.NewWriterPool(ctx, &pgCfg)
	if err != nil {
		return setupFailed(ctx, "postgres writer pool", err)
	}
	defer pgW.Close()

	var chCfg chdb.Config
	if err := envconfig.Process(ctx, &chCfg); err != nil {
		return setupFailed(ctx, "clickhouse config", err)
	}
	ch, err := chdb.NewReaderPool(ctx, &chCfg)
	if err != nil {
		return setupFailed(ctx, "clickhouse reader pool", err)
	}
	defer func() {
		if err := ch.Close(); err != nil {
			slog.WarnContext(ctx, "failed to close ClickHouse connection", slogx.Error(err))
		}
	}()

	j := &job{
		service:    coreusage.NewService(pgRO, pgW).WithClickHouse(ch),
		state:      cron.NewState(pgRO, pgW, cron.JobUsage),
		pgW:        pgW,
		rescanDays: rescanDays(ctx, cfg.RescanDays),
	}

	slog.InfoContext(ctx, "Running a usage metering pass", slog.Int("rescan_days", j.rescanDays))
	// Logged here rather than in main, which runs after closeOtel has swapped the
	// handler back to stderr.
	// Recorded at the layer that detected it; this only reports the disposition.
	outcome = outcomeFailed
	if err := j.run(ctx); err != nil {
		// Another pod is doing this work. Exit 0 — alerting on healthy overlap would
		// alert on nothing — but say so, because "skipped" and "metered" are
		// otherwise the same silent success.
		if errors.Is(err, cron.ErrLockHeld) {
			outcome = outcomeLockHeld
			slog.InfoContext(ctx, "another pass holds the usage lock; nothing to do")
			return nil
		}
		slog.ErrorContext(ctx, "usage metering pass failed", slogx.Error(err))
		// The root span's own status, not a second exception event: errors detected
		// under ClickHouse record onto its child span, and OTel does not propagate a
		// child's status to its parent — so a trace query filtered on status=ERROR
		// over usage.pass would miss exactly the passes that failed while metering.
		telemetry.RecordErrorOnSpan(span, err)
		return err
	}
	outcome = outcomeMetered
	if j.unrefreshed {
		outcome = outcomeUnverified
	}
	return nil
}

type job struct {
	service    *coreusage.Service
	state      *cron.State
	pgW        *pgxpool.Pool
	rescanDays int
	// Set when meter refreshed no period, so Run does not label the pass metered.
	unrefreshed bool
}

func (j *job) run(ctx context.Context) error {
	return cron.WithLock(ctx, j.pgW, cron.JobUsage, func(ctx context.Context) error {
		now := time.Now().UTC()
		// Both run even if the first fails, deliberately: prune deletes by age alone
		// and needs nothing the meter produces, so a ClickHouse outage should not
		// also stall retention. errors.Join keeps either failure in the exit code.
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
	var dropped int64
	if len(usage) == 0 {
		// An empty read is either a genuinely idle deployment or a ClickHouse this
		// pass cannot see properly. Stored cells over the same window settle it: an
		// idle deployment cannot have produced them.
		stored, err := j.service.CountStoredDays(ctx, from, now)
		if err != nil {
			return err
		}
		if stored > 0 {
			// Refreshing here would advance usage_computed_at over counts this pass
			// never verified -- and a stale stamp is the only layer that catches an
			// empty read, since the query itself succeeded. Skip every refresh so the
			// stamp does go stale, and record the error so OTLP sees it now. Still
			// exit 0: a transient bad read must not thrash the CronJob.
			err := fmt.Errorf("usage meter read no cells over [%s, %s) while %d stored cells exist",
				from.Format(time.RFC3339), now.Format(time.RFC3339), stored)
			slog.ErrorContext(ctx, "usage meter read nothing over stored days; refreshing no periods",
				slogx.Error(err))
			telemetry.RecordError(ctx, err)
			unrefreshedCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reasonUnverifiedRead)))
			j.unrefreshed = true
			return nil
		}
		slog.WarnContext(ctx, "usage meter read no cells and none are stored; treating the window as idle",
			slog.Time("from", from), slog.Time("to", now))
	} else {
		// Does this Postgres know the projects this ClickHouse just reported? An
		// unknown project's upsert writes nothing and reports success, so a pair
		// pointed at different environments meters thousands of cells, stores none,
		// and would then reconcile every stored cell away and stamp every org with a
		// zero. Counting stored rows cannot detect that -- cells written by earlier
		// correct passes are still there -- so ask directly.
		metered := coreusage.ProjectIDs(usage)
		known, err := j.service.CountKnownProjects(ctx, metered)
		if err != nil {
			return err
		}
		if known == 0 {
			// Same disposition as a suspicious empty read: reconcile nothing, refresh
			// nothing, record it, still exit 0 so a transient misconfiguration does not
			// thrash the CronJob. The stamp goes stale, which is the signal.
			err := fmt.Errorf("usage meter read %d cells over [%s, %s) for %d projects postgres does not know",
				len(usage), from.Format(time.RFC3339), now.Format(time.RFC3339), len(metered))
			slog.ErrorContext(ctx, "usage meter read cells for no known project; refreshing no periods",
				slogx.Error(err))
			telemetry.RecordError(ctx, err)
			unrefreshedCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reasonUnknownProjects)))
			j.unrefreshed = true
			return nil
		}
		if known < int64(len(metered)) {
			// Routine for a deleted project, so this reconciles anyway -- but it is
			// also what a partial mismatch looks like, and nothing else would say so.
			slog.WarnContext(ctx, "usage meter read cells for projects postgres does not know",
				slog.Int("metered_projects", len(metered)), slog.Int64("known_projects", known))
		}

		// Guarded here and again inside DeleteUnmeteredDays: with no cells to keep,
		// every stored row in the window counts as unmetered.
		if dropped, err = j.service.DeleteUnmeteredDays(ctx, usage, from, now); err != nil {
			return err
		}
		if dropped > 0 {
			// Recorded, not merely warned. Inside a healthy deployment the only cause
			// is an erasure or a dropped partition, but the same shape is what a
			// truncated read produces -- ClickHouse's *_overflow_mode='break' and an
			// unavailable shard both return partial results with no error, and the
			// query succeeding is why no other layer can see it. Non-fatal: a real
			// erasure still has to reconcile.
			err := fmt.Errorf("usage meter dropped %d stored day cells over [%s, %s) that clickhouse no longer returns",
				dropped, from.Format(time.RFC3339), now.Format(time.RFC3339))
			slog.WarnContext(ctx, "dropped usage days ClickHouse no longer returns", slogx.Error(err),
				slog.Int64("dropped", dropped), slog.Int("metered_cells", len(usage)),
				slog.Time("from", from), slog.Time("to", now))
			telemetry.RecordError(ctx, err)
			droppedDayCounter.Add(ctx, dropped)
		}
	}
	// Stamping a read we would not reconcile on defers the next wide window 24h.
	if full && len(usage) > 0 {
		if err := j.state.MarkRun(ctx, taskFullRecompute, now); err != nil {
			return err
		}
	}

	// No orgs at all usually means the reader pool is pointed somewhere unmigrated,
	// not that pug has no users. Metered cells settle it: a cell implies a project,
	// which has an FK to an org, so cells without orgs cannot happen on a Postgres
	// this pass can actually see. Fail rather than warn — left as a warning this
	// freezes every org's stamp forever behind a green CronJob, since the day cells
	// still land on the writer pool and no other guard fires.
	if len(windows) == 0 {
		if len(usage) > 0 {
			err := fmt.Errorf("usage meter found no orgs while metering %d cells", len(usage))
			slog.ErrorContext(ctx, "usage meter found no orgs to refresh", slogx.Error(err))
			telemetry.RecordError(ctx, err)
			return err
		}
		slog.WarnContext(ctx, "usage meter found no orgs to refresh; treating as a fresh deployment")
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
		// Each org's own failure was recorded at source; this reports the tally.
		err := fmt.Errorf("usage meter left %d of %d orgs unrefreshed: %w", failed, len(windows), firstErr)
		slog.ErrorContext(ctx, "failed to refresh period usage for some orgs", slogx.Error(err)) // puglint:exempt — each org's failure was recorded at source
		return err
	}

	slog.InfoContext(ctx, "metered event usage",
		slog.Time("from", from), slog.Int("cells", len(usage)),
		slog.Int64("dropped", dropped),
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
