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
	"strconv"
	"testing"

	"github.com/testcontainers/testcontainers-go"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/rs/xid"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

const testPostgresImage = "postgres@sha256:96d56f7f57c6aacd1fcb908bc83b345ec5f83231ee486dd66a1baadce274db88" // PostgreSQL 18.4-alpine

const (
	// pgTemplateDB holds the migrated schema every test database is cloned from.
	// Migrations run against it once per test binary, after which nothing may
	// connect: CREATE DATABASE ... TEMPLATE is refused while any session is
	// attached to the source. sealTemplate enforces that.
	pgTemplateDB = "pug_template"

	// pgAdminDB is the database the CREATE/DROP DATABASE statements are issued
	// from. It is neither the template nor a test database, so those statements
	// never run from a connection to their own target.
	pgAdminDB = "postgres"

	// pgPoolMaxConns caps each pool's backends. pgxpool otherwise defaults
	// MaxConns to max(4, NumCPU), so on a many-core machine a test's two pools
	// alone could approach the server's max_connections of 100 — which every test
	// in the package now shares rather than getting a server of its own.
	//
	// It is a ceiling, not a reservation: MinConns is 0, so a pool holds only what
	// it uses. 8 is the suite's widest fan-out — internal/core/auth's refresh and
	// magic-link races each run 8 goroutines through one pool — so all of them
	// reach the database together instead of queueing in Acquire for a connection,
	// which is the overlap those tests exist to create. Raise it alongside any
	// test that fans out wider.
	//
	// The ceiling that matters is one test's two pools plus the admin pool: 24
	// backends against 100, and a binary's tests run sequentially
	// (TestSetupCallersDoNotUseParallel), so no other test is holding any.
	pgPoolMaxConns = 8
)

// TestPostgres holds the connection pools for one test's private database.
type TestPostgres struct {
	PgRO *pgxpool.Pool
	PgW  *pgxpool.Pool
}

// sharedPostgres is the single container backing every test in the package.
type sharedPostgres struct {
	admin *pgxpool.Pool
	base  *url.URL
}

// plainDSN builds a connection string for db carrying no pgxpool-only
// parameters, for a plain pgx connection such as migrate's sql.Open.
//
// The two DSN builders are separate functions rather than one with a flag
// because each is wrong in a different way at the other's call site, and only
// one of the two mistakes is visible. See poolDSN.
func plainDSN(base *url.URL, db string) string {
	u := *base
	u.Path = "/" + db
	return u.String()
}

// poolDSN builds a connection string for db carrying the pgPoolMaxConns cap, for
// pgxpool.New.
//
// Handing it to a plain pgx connection announces itself: pgx forwards a key it
// does not recognize to the server as a runtime parameter, so pool_max_conns
// fails the connection outright (SQLSTATE 42704) rather than being ignored. The
// reverse — a plainDSN reaching pgxpool.New — is the quiet one: the pool builds
// fine and falls back to its max(4, NumCPU) default, which is the cap's entire
// reason for existing, and nothing says so.
func poolDSN(base *url.URL, db string) string {
	u := *base
	u.Path = "/" + db
	q := u.Query()
	q.Set("pool_max_conns", strconv.Itoa(pgPoolMaxConns))
	u.RawQuery = q.Encode()
	return u.String()
}

var pgContainer = &lazyContainer[sharedPostgres]{kind: "postgres", start: startPostgres}

// SetupPostgres returns read and write pools onto a freshly migrated database
// private to this test. Cleanup is registered via t.Cleanup.
//
// The container is started once per test binary and shared by the package's
// tests; each test's isolation comes from its own database cloned off a
// pre-migrated template, which costs milliseconds against the seconds a
// container start plus a migration run costs. Packages must wire up Main to
// tear the container down.
func SetupPostgres(t *testing.T) *TestPostgres {
	t.Helper()
	// Scoped to the test: t.Context is cancelled just before this test's cleanups
	// run, so a test that times out stops waiting on its own database too. The
	// shared container and the cleanup below deliberately do not use it — see
	// startPostgres and the t.Cleanup body.
	ctx := t.Context()

	sp := pgContainer.get(t)

	name := "test_" + xid.New().String()
	quoted := pgx.Identifier{name}.Sanitize()

	if _, err := sp.admin.Exec(ctx, fmt.Sprintf("create database %s template %s",
		quoted, pgx.Identifier{pgTemplateDB}.Sanitize())); err != nil {
		t.Fatalf("testutil: create test database: %v", err)
	}

	dsn := poolDSN(sp.base, name)

	pgRO, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("testutil: create read pool: %v", err)
	}

	pgW, err := pgxpool.New(ctx, dsn)
	if err != nil {
		pgRO.Close()
		t.Fatalf("testutil: create write pool: %v", err)
	}

	t.Cleanup(func() {
		pgRO.Close()
		pgW.Close()
		// Background, not t.Context: the test's context is already cancelled by
		// the time cleanups run, so the drop would never be sent and the database
		// would linger in the shared container for the rest of the package.
		//
		// force: pgxpool releases connections asynchronously, and a backend that
		// outlives the pool holds the database open against a plain drop.
		if _, err := sp.admin.Exec(context.Background(),
			fmt.Sprintf("drop database if exists %s with (force)", quoted)); err != nil {
			t.Errorf("testutil: drop test database %s: %v", name, err)
		}
	})

	return &TestPostgres{PgRO: pgRO, PgW: pgW}
}

func startPostgres() (_ *sharedPostgres, err error) {
	// Background, not the triggering test's t.Context: this container is shared by
	// the whole package and outlives whichever test happened to start it, so
	// binding it to that test's context would tear it down under the next one.
	ctx := context.Background()

	ctr, err := tcpostgres.Run(ctx, testPostgresImage,
		tcpostgres.WithDatabase(pgTemplateDB),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"),
		tcpostgres.WithSQLDriver("pgx"),
		tcpostgres.BasicWaitStrategies(),
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

	// ConnectionString, not MustConnectionString: the Must variant panics, and a
	// panic here would carry err past the defer above still nil, so the container
	// it just decided it could not describe would never be terminated.
	connStr, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return nil, fmt.Errorf("connection string: %w", err)
	}

	base, err := url.Parse(connStr)
	if err != nil {
		return nil, fmt.Errorf("parse connection string: %w", err)
	}
	// Migrations run against the template exactly once; every test database is a
	// copy of the result. This is also the only connection the template ever takes
	// — see sealTemplate, which closes it to any other.
	if err = migrate(ctx, plainDSN(base, pgTemplateDB)); err != nil {
		return nil, fmt.Errorf("migrate template: %w", err)
	}

	admin, err := pgxpool.New(ctx, poolDSN(base, pgAdminDB))
	if err != nil {
		return nil, fmt.Errorf("admin pool: %w", err)
	}

	if err = sealTemplate(ctx, admin); err != nil {
		admin.Close()
		return nil, err
	}

	teardowns.add(func() error {
		admin.Close()
		if err := testcontainers.TerminateContainer(ctr); err != nil {
			return fmt.Errorf("terminate postgres container: %w", err)
		}
		return nil
	})

	return &sharedPostgres{admin: admin, base: base}, nil
}

// sealTemplate locks the template down once it is migrated. CREATE DATABASE ...
// TEMPLATE fails outright while any session is attached to the source (SQLSTATE
// 55006), and migrate above is the only thing that ever connects to it — the
// readiness probe waits on a log line and a listening port, never opening a
// session.
//
// database/sql closes its connections inline, so in practice the template is
// already idle by the time this runs. Barring new connections turns that into a
// guarantee, and turns a future stray connection to the template into a loud
// error at the point of the mistake rather than a clone that fails
// intermittently. Evicting whatever is still attached covers the remainder: the
// two-argument pg_terminate_backend waits for the backend to exit, where the
// one-argument form returns once the signal is merely sent and would leave the
// first clone racing a dying session.
func sealTemplate(ctx context.Context, admin *pgxpool.Pool) error {
	quoted := pgx.Identifier{pgTemplateDB}.Sanitize()

	if _, err := admin.Exec(ctx, fmt.Sprintf("alter database %s with allow_connections false", quoted)); err != nil {
		return fmt.Errorf("bar connections to template: %w", err)
	}
	if _, err := admin.Exec(ctx,
		"select pg_terminate_backend(pid, 5000) from pg_stat_activity where datname = $1 and pid <> pg_backend_pid()",
		pgTemplateDB); err != nil {
		return fmt.Errorf("evict template sessions: %w", err)
	}
	return nil
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
