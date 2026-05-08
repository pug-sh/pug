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
	"github.com/pressly/goose/v3"
	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	chdep "github.com/pug-sh/pug/internal/deps/clickhouse"
)

const testClickHouseImage = "clickhouse/clickhouse-server@sha256:b98554227ed543d21d2cd632df77fb2060454a2caeab47929fa6118c9f8bbe2f" // 26.3.9.8-alpine

// TestClickHouse holds the container and connection for a test ClickHouse instance.
type TestClickHouse struct {
	container *tcclickhouse.ClickHouseContainer
	Conn      *chdep.Conn
	URL       string
}

// SetupClickHouse starts a ClickHouse container, runs all goose migrations,
// and returns a connection. Cleanup is registered via t.Cleanup.
func SetupClickHouse(t *testing.T) *TestClickHouse {
	t.Helper()
	ctx := context.Background()

	ctr, err := tcclickhouse.Run(ctx, testClickHouseImage,
		tcclickhouse.WithDatabase("pug_test"),
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

	chConn, err := chdep.NewReaderPool(ctx, &chdep.Config{URL: connStr})
	if err != nil {
		_ = ctr.Terminate(ctx)
		t.Fatalf("testutil: create clickhouse connection: %v", err)
	}

	tc := &TestClickHouse{container: ctr, Conn: chConn, URL: connStr}

	t.Cleanup(func() {
		_ = chConn.Close()
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

	ctr, err := tcclickhouse.Run(ctx, testClickHouseImage,
		tcclickhouse.WithDatabase("pug_test"),
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
