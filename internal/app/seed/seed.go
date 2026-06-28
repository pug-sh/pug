// Package seed orchestrates the `pug seed` command: it resets the data stores
// (or clears just the demo rows) and runs the same event-gated demo flow as the
// demo worker (internal/app/workers/demo). The CLI layer (cmd/pug) only parses
// flags and calls Run, keeping the orchestration testable and out of main.
package seed

import (
	"context"
	"fmt"
	"log/slog"

	migrateclickhouse "github.com/pug-sh/pug/internal/app/migrate/clickhouse"
	migratepostgres "github.com/pug-sh/pug/internal/app/migrate/postgres"
	chseed "github.com/pug-sh/pug/internal/app/seed/clickhouse"
	pgseed "github.com/pug-sh/pug/internal/app/seed/postgres"
	chdeps "github.com/pug-sh/pug/internal/deps/clickhouse"
	pgdeps "github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/slogx"
	"github.com/sethvargo/go-envconfig"
)

// Options configures a seed run. The CLI populates these from its flags.
type Options struct {
	Count     int64 // total number of events to generate
	BatchSize int   // events per ClickHouse insert batch
	NoReset   bool  // skip migrate down/up; clear the demo tables and re-seed instead
}

// Run resets the stores (unless NoReset) and seeds the demo project. By default
// it rolls both stores' migrations down then up so they start from a clean
// schema; NoReset leaves the schema in place and clears just the demo rows
// before re-seeding (so a re-seed never pairs fresh events with a stale profile
// set).
func Run(ctx context.Context, opts Options) error {
	// Clear the existing demo rows only on the NoReset path: the reset path drops
	// and recreates the schema, so it already starts empty. That condition is
	// exactly opts.NoReset, so bind it directly rather than defaulting and flipping.
	clearDemoRows := opts.NoReset
	if !opts.NoReset {
		if err := resetStores(ctx); err != nil {
			return err
		}
	}
	return seedDemoData(ctx, opts.Count, opts.BatchSize, clearDemoRows)
}

// resetStores rolls Postgres and ClickHouse migrations all the way down then
// back up, so the seed starts from a clean schema.
func resetStores(ctx context.Context) error {
	for _, m := range []struct {
		name string
		down func(context.Context, int) error
		up   func(context.Context, int) error
	}{
		{"postgres", migratepostgres.Down, migratepostgres.Up},
		{"clickhouse", migrateclickhouse.Down, migrateclickhouse.Up},
	} {
		slog.InfoContext(ctx, "resetting migrations", slog.String("store", m.name))
		if err := m.down(ctx, 0); err != nil {
			return fmt.Errorf("migrate %s down: %w", m.name, err)
		}
		if err := m.up(ctx, 0); err != nil {
			return fmt.Errorf("migrate %s up: %w", m.name, err)
		}
	}
	return nil
}

// seedDemoData runs the same event-gated flow as the demo worker
// (internal/app/workers/demo): ensure the demo account, backfill events, seed
// Postgres profiles for exactly the users that produced events, then copy those
// profiles into ClickHouse. When clearDemoRows is set (the NoReset path) it
// first clears the demo events + profiles from ClickHouse and the demo profiles
// from Postgres, so a re-seed starts clean instead of pairing fresh events with
// stale profiles.
func seedDemoData(ctx context.Context, count int64, batchSize int, clearDemoRows bool) error {
	var pgCfg pgdeps.Config
	if err := envconfig.Process(ctx, &pgCfg); err != nil {
		return err
	}
	pg, err := pgdeps.NewWriterPool(ctx, &pgCfg)
	if err != nil {
		return err
	}
	defer pg.Close()

	var chCfg chdeps.Config
	if err := envconfig.Process(ctx, &chCfg); err != nil {
		return err
	}
	chDB, err := chdeps.NewFromConfig(ctx, &chCfg)
	if err != nil {
		return err
	}
	defer func() {
		if err := chDB.Conn.Close(); err != nil {
			slog.WarnContext(ctx, "failed to close ClickHouse connection", slogx.Error(err))
		}
	}()
	ch := chDB.Conn

	project, err := pgseed.SeedAccount(ctx, pg)
	if err != nil {
		return fmt.Errorf("seed account: %w", err)
	}

	if clearDemoRows {
		// events and profiles are shared tables keyed by project_id, so scope the
		// wipe to the demo project — a blanket TRUNCATE would delete every other
		// project's data. ALTER ... DELETE WHERE project_id mirrors the Postgres
		// ResetDemoProfiles below and the compliance worker's erasure path;
		// mutations_sync = 1 blocks until the rows are physically gone so the
		// re-backfill that follows can't race stale survivors (events carry fresh
		// ids each run, so leftovers would double-count rather than dedup).
		for _, table := range []string{"events", "profiles"} {
			slog.InfoContext(ctx, "clearing clickhouse demo rows",
				slog.String("table", table), slog.String("project_id", project.ID))
			query := fmt.Sprintf("ALTER TABLE %s DELETE WHERE project_id = ? SETTINGS mutations_sync = 1", table)
			if err := ch.Exec(ctx, query, project.ID); err != nil {
				return fmt.Errorf("clear %s for project %s: %w", table, project.ID, err)
			}
		}
		// Clear the Postgres demo profiles too. Without this, the leftover rows
		// trip seedProfilesForUsers' idempotency skip and the fresh backfill's
		// events get paired with the stale profile set (orphan profiles/events).
		slog.InfoContext(ctx, "clearing postgres demo profiles", slog.String("project_id", project.ID))
		if err := pgseed.ResetDemoProfiles(ctx, pg, project.ID); err != nil {
			return err
		}
	}

	indices, err := chseed.BackfillEvents(ctx, ch, project.ID, count, batchSize)
	if err != nil {
		return fmt.Errorf("backfill events: %w", err)
	}
	if err := pgseed.SeedProfilesForUsers(ctx, pg, project.ID, indices); err != nil {
		return fmt.Errorf("seed profiles: %w", err)
	}
	if err := chseed.CopyProfilesToClickHouse(ctx, pg, ch, project.ID); err != nil {
		return fmt.Errorf("copy profiles to clickhouse: %w", err)
	}

	// Saved showcase dashboards are static config (they reference event kinds /
	// properties, not rows), so they are ensured idempotently after the data is
	// in place rather than cleared/recreated on the --no-reset path.
	if err := pgseed.SeedDemoDashboards(ctx, pg, project.ID); err != nil {
		return fmt.Errorf("seed demo dashboards: %w", err)
	}

	slog.InfoContext(ctx, "demo seed complete",
		slog.String("project_id", project.ID),
		slog.Int("profiles", len(indices)),
	)
	return nil
}
