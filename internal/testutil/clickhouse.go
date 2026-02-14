package testutil

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"runtime"

	_ "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/pressly/goose/v3"
	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	chdep "github.com/fivebitsio/cotton/internal/deps/clickhouse"
)

// TestClickHouse holds the container and connection for a test ClickHouse instance.
type TestClickHouse struct {
	container *tcclickhouse.ClickHouseContainer
	Conn      driver.Conn
	URL       string
}

// SetupClickHouse starts a ClickHouse container, runs all goose migrations,
// and returns a connection. Call Close when done.
//
// Panics on any setup failure.
func SetupClickHouse() *TestClickHouse {
	ctx := context.Background()

	ctr, err := tcclickhouse.Run(ctx, "clickhouse/clickhouse-server:24-alpine",
		tcclickhouse.WithDatabase("cotton_test"),
		tcclickhouse.WithUsername("default"),
		tcclickhouse.WithPassword(""),
	)
	if err != nil {
		panic(fmt.Sprintf("testutil: start clickhouse container: %v", err))
	}

	connStr, err := ctr.ConnectionString(ctx)
	if err != nil {
		_ = ctr.Terminate(ctx)
		panic(fmt.Sprintf("testutil: get clickhouse connection string: %v", err))
	}

	if err := migrateClickHouse(ctx, connStr); err != nil {
		_ = ctr.Terminate(ctx)
		panic(fmt.Sprintf("testutil: run clickhouse migrations: %v", err))
	}

	conn, err := chdep.NewReaderPool(ctx, &chdep.Config{URL: connStr})
	if err != nil {
		_ = ctr.Terminate(ctx)
		panic(fmt.Sprintf("testutil: create clickhouse connection: %v", err))
	}

	return &TestClickHouse{container: ctr, Conn: conn, URL: connStr}
}

// SetupBareClickHouse starts a ClickHouse container without running migrations.
// Use this when testing migration logic directly. Call Close when done.
//
// Panics on any setup failure.
func SetupBareClickHouse() *TestClickHouse {
	ctx := context.Background()

	ctr, err := tcclickhouse.Run(ctx, "clickhouse/clickhouse-server:24-alpine",
		tcclickhouse.WithDatabase("cotton_test"),
		tcclickhouse.WithUsername("default"),
		tcclickhouse.WithPassword(""),
	)
	if err != nil {
		panic(fmt.Sprintf("testutil: start clickhouse container: %v", err))
	}

	connStr, err := ctr.ConnectionString(ctx)
	if err != nil {
		_ = ctr.Terminate(ctx)
		panic(fmt.Sprintf("testutil: get clickhouse connection string: %v", err))
	}

	return &TestClickHouse{container: ctr, URL: connStr}
}

// Close tears down the connection and the container.
func (tc *TestClickHouse) Close() {
	if tc.Conn != nil {
		_ = tc.Conn.Close()
	}
	_ = tc.container.Terminate(context.Background())
}

func migrateClickHouse(ctx context.Context, connStr string) error {
	db, err := sql.Open("clickhouse", connStr)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	if err := goose.SetDialect("clickhouse"); err != nil {
		return err
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return fmt.Errorf("unable to determine source file path")
	}
	dir := filepath.Join(filepath.Dir(thisFile), "..", "..", "schema", "clickhouse", "migrations")

	return goose.UpContext(ctx, db, dir)
}
