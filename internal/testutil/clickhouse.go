package testutil

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"

	_ "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/chcol"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/pressly/goose/v3"
	"github.com/rs/xid"
	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	chq "github.com/pug-sh/pug/internal/core/clickhouse"
	chdep "github.com/pug-sh/pug/internal/deps/clickhouse"
)

const testClickHouseImage = "clickhouse/clickhouse-server@sha256:a4202d2f7a0c0ac98aca2bda670f5b7278722463c0bdd4c2de5241e8e1e898ed" // 26.5.1.882-alpine

// chAdminDB is the container's initial database, used only to issue the
// CREATE/DROP DATABASE statements for the per-test databases. Tests never read
// or write it. ClickHouse has no CREATE DATABASE ... TEMPLATE, so unlike
// Postgres each test database is migrated on creation rather than cloned.
const chAdminDB = "pug_admin"

// TestClickHouse holds the connection to one test's private database.
type TestClickHouse struct {
	Conn *chdep.Conn
	URL  string
}

// sharedClickHouse is the single container backing every test in the package.
type sharedClickHouse struct {
	admin   *sql.DB
	connFor func(db string) string
}

var chContainer = &lazyContainer[sharedClickHouse]{kind: "clickhouse", start: startClickHouse}

// SetupClickHouse starts (or reuses) the package's ClickHouse container, then
// creates and migrates a database private to this test. Cleanup is registered
// via t.Cleanup; the container is torn down by Main.
func SetupClickHouse(t *testing.T) *TestClickHouse {
	t.Helper()
	// Scoped to the test — see the note in SetupPostgres for why the shared
	// container and the cleanup below use context.Background instead.
	ctx := t.Context()

	sc := chContainer.get(t)

	name := "test_" + xid.New().String()
	if _, err := sc.admin.ExecContext(ctx, fmt.Sprintf("create database %s", quoteCH(name))); err != nil {
		t.Fatalf("testutil: create clickhouse database: %v", err)
	}

	// Registered before the migration and connection so a failure in either still
	// drops the database; otherwise it would linger in the shared container for
	// the rest of the package. The connection is closed only once set.
	var conn *chdep.Conn
	t.Cleanup(func() {
		if conn != nil {
			_ = conn.Close()
		}
		// Background, not t.Context: the test's context is already cancelled once
		// cleanups run, so the drop would never be sent.
		//
		// sync: the databases here are Atomic, which detaches a dropped table at once
		// but only unlinks its data after database_atomic_delay_before_drop_table_sec
		// (480s). The database leaves system.databases either way, so nothing looks
		// wrong — but without sync every test's tables sit in system.dropped_tables
		// holding their data, and a package's worth of them accumulates in a container
		// the whole package shares. sync unlinks before returning.
		if _, err := sc.admin.ExecContext(context.Background(),
			fmt.Sprintf("drop database if exists %s sync", quoteCH(name))); err != nil {
			t.Errorf("testutil: drop clickhouse database %s: %v", name, err)
		}
	})

	dsn := sc.connFor(name)
	if err := migrateClickHouse(ctx, dsn); err != nil {
		t.Fatalf("testutil: run clickhouse migrations: %v", err)
	}

	conn, err := chdep.NewReaderPool(ctx, &chdep.Config{URL: dsn})
	if err != nil {
		t.Fatalf("testutil: create clickhouse connection: %v", err)
	}

	return &TestClickHouse{Conn: conn, URL: dsn}
}

func startClickHouse() (_ *sharedClickHouse, err error) {
	// Background, not the triggering test's t.Context: this container is shared by
	// the whole package and outlives whichever test started it.
	ctx := context.Background()

	ctr, err := tcclickhouse.Run(ctx, testClickHouseImage,
		tcclickhouse.WithDatabase(chAdminDB),
		tcclickhouse.WithUsername("default"),
		tcclickhouse.WithPassword("test"),
	)
	// Run hands back a created-but-never-ready container alongside its error when
	// the readiness probe times out, so every failure path below has to terminate
	// it. TerminateContainer tolerates a nil container.
	defer func() {
		if err != nil {
			err = errors.Join(err, testcontainers.TerminateContainer(ctr))
		}
	}()
	if err != nil {
		return nil, fmt.Errorf("start container: %w", err)
	}

	connStr, err := ctr.ConnectionString(ctx)
	if err != nil {
		return nil, fmt.Errorf("connection string: %w", err)
	}

	base, err := url.Parse(connStr)
	if err != nil {
		return nil, fmt.Errorf("parse connection string: %w", err)
	}
	connFor := func(db string) string {
		u := *base
		u.Path = "/" + db
		return u.String()
	}

	admin, err := sql.Open("clickhouse", connFor(chAdminDB))
	if err != nil {
		return nil, fmt.Errorf("admin connection: %w", err)
	}

	teardowns.add(func() error {
		_ = admin.Close()
		if err := testcontainers.TerminateContainer(ctr); err != nil {
			return fmt.Errorf("terminate clickhouse container: %w", err)
		}
		return nil
	})

	return &sharedClickHouse{admin: admin, connFor: connFor}, nil
}

// quoteCH quotes a ClickHouse identifier. The names it is given are generated,
// not caller-supplied, so this is hygiene rather than a trust boundary.
func quoteCH(ident string) string {
	return "`" + ident + "`"
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
