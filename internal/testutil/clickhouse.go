package testutil

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	_ "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/chcol"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/pressly/goose/v3"
	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	chq "github.com/pug-sh/pug/internal/core/clickhouse"
	chdep "github.com/pug-sh/pug/internal/deps/clickhouse"
)

const testClickHouseImage = "clickhouse/clickhouse-server@sha256:a4202d2f7a0c0ac98aca2bda670f5b7278722463c0bdd4c2de5241e8e1e898ed" // 26.5.1.882-alpine

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

// InsertEvent inserts one row into the ClickHouse events table, routing
// promoted auto-properties to dedicated columns and storing the remainder in
// auto_properties.
func InsertEvent(
	ctx context.Context,
	t *testing.T,
	conn driver.Conn,
	eventID, projectID, distinctID, kind, sessionID string,
	auto, custom map[string]string,
	occurTime time.Time,
) {
	t.Helper()

	autoAny := make(map[string]any, len(auto))
	for k, v := range auto {
		autoAny[k] = v
	}
	customAny := make(map[string]any, len(custom))
	for k, v := range custom {
		customAny[k] = v
	}

	promoted, restAuto := chq.SplitPromotedAutoAnyProperties(autoAny)
	autoVariants := stringMapToVariantMap(restAuto)
	customVariants := stringMapToVariantMap(customAny)

	batch, err := conn.PrepareBatch(ctx, chq.EventsInsertStmt)
	if err != nil {
		t.Fatalf("prepare event insert batch: %v", err)
	}

	args := []any{eventID, projectID, distinctID, kind, autoVariants, customVariants}
	args = append(args, promoted.AppendArgs()...)
	args = append(args, occurTime, sessionID)
	if err := batch.Append(args...); err != nil {
		t.Fatalf("append event insert batch: %v", err)
	}
	if err := batch.Send(); err != nil {
		t.Fatalf("send event insert batch: %v", err)
	}
}

func stringMapToVariantMap(src map[string]any) map[string]chcol.Variant {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]chcol.Variant, len(src))
	for k, v := range src {
		s, ok := v.(string)
		if !ok {
			continue
		}
		out[k] = chcol.NewVariantWithType(s, "String")
	}
	return out
}
