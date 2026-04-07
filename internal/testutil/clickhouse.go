package testutil

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

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
// and returns a connection. Cleanup is registered via t.Cleanup.
func SetupClickHouse(t *testing.T) *TestClickHouse {
	t.Helper()
	ctx := context.Background()

	ctr, err := tcclickhouse.Run(ctx, "clickhouse/clickhouse-server:26.3-alpine",
		tcclickhouse.WithDatabase("cotton_test"),
		tcclickhouse.WithUsername("default"),
		tcclickhouse.WithPassword("test"),
	)
	if err != nil {
		t.Fatalf("testutil: start clickhouse container: %v", err)
	}

	connStr, err := ctr.ConnectionString(ctx)
	if err != nil {
		_ = ctr.Terminate(ctx)
		t.Fatalf("testutil: get clickhouse connection string: %v", err)
	}

	if err := migrateClickHouse(ctx, connStr); err != nil {
		_ = ctr.Terminate(ctx)
		t.Fatalf("testutil: run clickhouse migrations: %v", err)
	}

	conn, err := chdep.NewReaderPool(ctx, &chdep.Config{URL: connStr})
	if err != nil {
		_ = ctr.Terminate(ctx)
		t.Fatalf("testutil: create clickhouse connection: %v", err)
	}

	tc := &TestClickHouse{container: ctr, Conn: conn, URL: connStr}

	t.Cleanup(func() {
		_ = conn.Close()
		if err := ctr.Terminate(context.Background()); err != nil {
			fmt.Printf("testutil: terminate clickhouse container: %v\n", err)
		}
	})

	return tc
}

// SetupBareClickHouse starts a ClickHouse container without running migrations.
// Use this when testing migration logic directly. Cleanup is registered via t.Cleanup.
func SetupBareClickHouse(t *testing.T) *TestClickHouse {
	t.Helper()
	ctx := context.Background()

	ctr, err := tcclickhouse.Run(ctx, "clickhouse/clickhouse-server:26.3-alpine",
		tcclickhouse.WithDatabase("cotton_test"),
		tcclickhouse.WithUsername("default"),
		tcclickhouse.WithPassword("test"),
	)
	if err != nil {
		t.Fatalf("testutil: start clickhouse container: %v", err)
	}

	connStr, err := ctr.ConnectionString(ctx)
	if err != nil {
		_ = ctr.Terminate(ctx)
		t.Fatalf("testutil: get clickhouse connection string: %v", err)
	}

	tc := &TestClickHouse{container: ctr, URL: connStr}

	t.Cleanup(func() {
		if err := ctr.Terminate(context.Background()); err != nil {
			fmt.Printf("testutil: terminate clickhouse container: %v\n", err)
		}
	})

	return tc
}

func migrateClickHouse(ctx context.Context, connStr string) error {
	db, err := sql.Open("clickhouse", connStr)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return fmt.Errorf("unable to determine source file path")
	}
	dir := filepath.Join(filepath.Dir(thisFile), "..", "..", "schema", "clickhouse", "migrations")

	provider, err := goose.NewProvider(goose.DialectClickHouse, db, os.DirFS(dir))
	if err != nil {
		return err
	}

	_, err = provider.Up(ctx)
	return err
}
