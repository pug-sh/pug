package testutil

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// TestPostgres holds the container and connection pools for a test database.
type TestPostgres struct {
	container *tcpostgres.PostgresContainer
	ConnStr   string
	PgRO      *pgxpool.Pool
	PgW       *pgxpool.Pool
}

// SetupPostgres starts a PostgreSQL container, runs all goose migrations,
// and returns connection pools. Cleanup is registered via t.Cleanup.
func SetupPostgres(t *testing.T) *TestPostgres {
	t.Helper()
	ctx := context.Background()

	ctr, err := tcpostgres.Run(ctx, "postgres:18-alpine",
		tcpostgres.WithDatabase("pug_test"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"),
		tcpostgres.WithSQLDriver("pgx"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("testutil: start postgres container: %v", err)
	}

	connStr := ctr.MustConnectionString(ctx, "sslmode=disable")

	if err := migrate(ctx, connStr); err != nil {
		_ = ctr.Terminate(ctx)
		t.Fatalf("testutil: run migrations: %v", err)
	}

	pgRO, err := pgxpool.New(ctx, connStr)
	if err != nil {
		_ = ctr.Terminate(ctx)
		t.Fatalf("testutil: create read pool: %v", err)
	}

	pgW, err := pgxpool.New(ctx, connStr)
	if err != nil {
		pgRO.Close()
		_ = ctr.Terminate(ctx)
		t.Fatalf("testutil: create write pool: %v", err)
	}

	tp := &TestPostgres{container: ctr, ConnStr: connStr, PgRO: pgRO, PgW: pgW}

	t.Cleanup(func() {
		pgRO.Close()
		pgW.Close()
		if err := ctr.Terminate(context.Background()); err != nil {
			fmt.Printf("testutil: terminate postgres container: %v\n", err)
		}
	})

	return tp
}

// SetupBarePostgres starts a PostgreSQL container without running migrations.
// Use this when testing migration logic directly. Cleanup is registered via t.Cleanup.
func SetupBarePostgres(t *testing.T) *TestPostgres {
	t.Helper()
	ctx := context.Background()

	ctr, err := tcpostgres.Run(ctx, "postgres:18-alpine",
		tcpostgres.WithDatabase("pug_test"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"),
		tcpostgres.WithSQLDriver("pgx"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("testutil: start postgres container: %v", err)
	}

	connStr := ctr.MustConnectionString(ctx, "sslmode=disable")

	tp := &TestPostgres{container: ctr, ConnStr: connStr}

	t.Cleanup(func() {
		if err := ctr.Terminate(context.Background()); err != nil {
			fmt.Printf("testutil: terminate postgres container: %v\n", err)
		}
	})

	return tp
}

func migrate(ctx context.Context, connStr string) error {
	db, err := sql.Open("pgx", connStr)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return fmt.Errorf("unable to determine source file path")
	}
	dir := filepath.Join(filepath.Dir(thisFile), "..", "..", "schema", "postgres", "migrations")

	provider, err := goose.NewProvider(goose.DialectPostgres, db, os.DirFS(dir))
	if err != nil {
		return err
	}

	_, err = provider.Up(ctx)
	return err
}
