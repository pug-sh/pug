package postgres

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/fivebitsio/cotton/internal/deps/postgres"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/sethvargo/go-envconfig"
)

func Up(ctx context.Context, num int) error {
	db, dir, err := setup(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	if num == 0 {
		if err := goose.UpContext(ctx, db, dir); err != nil {
			return err
		}
		slog.InfoContext(ctx, "applied all migrations")
		return nil
	}

	current, err := goose.GetDBVersionContext(ctx, db)
	if err != nil {
		return err
	}

	if err := goose.UpToContext(ctx, db, dir, current+int64(num)); err != nil {
		return err
	}

	slog.InfoContext(ctx, "applied migrations", slog.Int("appliedMigrations", num))
	return nil
}

func Down(ctx context.Context, num int) error {
	db, dir, err := setup(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	if num == 0 {
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
		slog.InfoContext(ctx, "rolled back all migrations")
		return nil
	}

	for i := 0; i < num; i++ {
		if err := goose.DownContext(ctx, db, dir); err != nil {
			return err
		}
	}

	slog.InfoContext(ctx, "rolled back migrations", slog.Int("rolledBackMigrations", num))
	return nil
}

func setup(ctx context.Context) (*sql.DB, string, error) {
	var cfg postgres.Config
	if err := envconfig.Process(ctx, &cfg); err != nil {
		return nil, "", err
	}

	db, err := sql.Open("pgx", cfg.URL)
	if err != nil {
		return nil, "", err
	}

	wd, err := os.Getwd()
	if err != nil {
		db.Close()
		return nil, "", err
	}

	if err := goose.SetDialect("postgres"); err != nil {
		db.Close()
		return nil, "", err
	}

	return db, filepath.Join(wd, "schema", "postgres", "migrations"), nil
}
