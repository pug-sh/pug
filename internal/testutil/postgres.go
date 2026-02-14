package testutil

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"path/filepath"
	"runtime"
)

// TestPostgres holds the container and connection pools for a test database.
type TestPostgres struct {
	container *tcpostgres.PostgresContainer
	ConnStr   string
	PgRO      *pgxpool.Pool
	PgW       *pgxpool.Pool
}

// SetupPostgres starts a PostgreSQL container, runs all goose migrations,
// and returns connection pools. Call Close when done.
//
// Panics on any setup failure.
func SetupPostgres() *TestPostgres {
	ctx := context.Background()

	ctr, err := tcpostgres.Run(ctx, "postgres:18-alpine",
		tcpostgres.WithDatabase("cotton_test"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"),
		tcpostgres.WithSQLDriver("pgx"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		panic(fmt.Sprintf("testutil: start postgres container: %v", err))
	}

	connStr := ctr.MustConnectionString(ctx, "sslmode=disable")

	if err := migrate(ctx, connStr); err != nil {
		_ = ctr.Terminate(ctx)
		panic(fmt.Sprintf("testutil: run migrations: %v", err))
	}

	pgRO, err := pgxpool.New(ctx, connStr)
	if err != nil {
		_ = ctr.Terminate(ctx)
		panic(fmt.Sprintf("testutil: create read pool: %v", err))
	}

	pgW, err := pgxpool.New(ctx, connStr)
	if err != nil {
		pgRO.Close()
		_ = ctr.Terminate(ctx)
		panic(fmt.Sprintf("testutil: create write pool: %v", err))
	}

	return &TestPostgres{container: ctr, ConnStr: connStr, PgRO: pgRO, PgW: pgW}
}

// SetupBarePostgres starts a PostgreSQL container without running migrations.
// Use this when testing migration logic directly. Call Close when done.
//
// Panics on any setup failure.
func SetupBarePostgres() *TestPostgres {
	ctx := context.Background()

	ctr, err := tcpostgres.Run(ctx, "postgres:18-alpine",
		tcpostgres.WithDatabase("cotton_test"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"),
		tcpostgres.WithSQLDriver("pgx"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		panic(fmt.Sprintf("testutil: start postgres container: %v", err))
	}

	connStr := ctr.MustConnectionString(ctx, "sslmode=disable")

	return &TestPostgres{container: ctr, ConnStr: connStr}
}

// Close tears down pools and the container.
func (tp *TestPostgres) Close() {
	if tp.PgRO != nil {
		tp.PgRO.Close()
	}
	if tp.PgW != nil {
		tp.PgW.Close()
	}
	_ = tp.container.Terminate(context.Background())
}

func migrate(ctx context.Context, connStr string) error {
	db, err := sql.Open("pgx", connStr)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return fmt.Errorf("unable to determine source file path")
	}
	dir := filepath.Join(filepath.Dir(thisFile), "..", "..", "schema", "postgres", "migrations")

	return goose.UpContext(ctx, db, dir)
}
