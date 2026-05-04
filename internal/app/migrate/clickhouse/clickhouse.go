package clickhouse

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"

	_ "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/pressly/goose/v3"
	clickhousedeps "github.com/pug-sh/pug/internal/deps/clickhouse"
	"github.com/sethvargo/go-envconfig"
)

func Up(ctx context.Context, num int) error {
	db, dir, err := setup(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	if num == 0 {
		if err := goose.UpContext(ctx, db, dir); err != nil {
			return err
		}
		slog.InfoContext(ctx, "applied all clickhouse migrations")
		return nil
	}

	current, err := goose.GetDBVersionContext(ctx, db)
	if err != nil {
		return err
	}

	if err := goose.UpToContext(ctx, db, dir, current+int64(num)); err != nil {
		return err
	}

	slog.InfoContext(ctx, "applied clickhouse migrations", slog.Int("applied_migrations", num))
	return nil
}

func Down(ctx context.Context, num int) error {
	db, dir, err := setup(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	if num == 0 {
		if err := pruneOrphanedVersions(ctx, db, dir); err != nil {
			return err
		}
		for {
			current, err := goose.GetDBVersionContext(ctx, db)
			if err != nil {
				return err
			}
			if current == 0 {
				break
			}
			if err := goose.DownContext(ctx, db, dir); err != nil {
				return err
			}
		}
		slog.InfoContext(ctx, "rolled back all clickhouse migrations")
		return nil
	}

	for i := 0; i < num; i++ {
		if err := goose.DownContext(ctx, db, dir); err != nil {
			return err
		}
	}

	slog.InfoContext(ctx, "rolled back clickhouse migrations", slog.Int("rolled_back_migrations", num))
	return nil
}

// pruneOrphanedVersions removes applied version entries from goose_db_version
// that have no corresponding migration file in dir. This handles the case where
// migration files were deleted or are on a different branch, allowing Down to
// proceed normally from the highest available version.
func pruneOrphanedVersions(ctx context.Context, db *sql.DB, dir string) error {
	migrations, err := goose.CollectMigrations(dir, 0, goose.MaxVersion)
	if err != nil {
		return err
	}

	available := make(map[int64]struct{}, len(migrations))
	var maxAvailable int64
	for _, m := range migrations {
		available[m.Version] = struct{}{}
		if m.Version > maxAvailable {
			maxAvailable = m.Version
		}
	}

	current, err := goose.GetDBVersionContext(ctx, db)
	if err != nil {
		return err
	}

	if current <= maxAvailable {
		return nil
	}

	slog.InfoContext(ctx, "pruning orphaned migration versions",
		slog.Int64("db_version", current),
		slog.Int64("max_available", maxAvailable),
	)
	_, err = db.ExecContext(ctx,
		"DELETE FROM goose_db_version WHERE version_id > ?", maxAvailable)
	return err
}

func setup(ctx context.Context) (*sql.DB, string, error) {
	var cfg clickhousedeps.Config
	if err := envconfig.Process(ctx, &cfg); err != nil {
		return nil, "", err
	}

	db, err := sql.Open("clickhouse", cfg.URL)
	if err != nil {
		return nil, "", err
	}

	wd, err := os.Getwd()
	if err != nil {
		_ = db.Close()
		return nil, "", err
	}

	if err := goose.SetDialect("clickhouse"); err != nil {
		_ = db.Close()
		return nil, "", err
	}

	return db, filepath.Join(wd, "schema", "clickhouse", "migrations"), nil
}
