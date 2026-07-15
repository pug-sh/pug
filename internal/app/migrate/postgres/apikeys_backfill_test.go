package postgres_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	coreprojects "github.com/pug-sh/pug/internal/core/projects"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/testutil"
)

// versionBeforeApiKeys is the last migration before 017_create_api_keys.
const versionBeforeApiKeys = 16

// sha256Hex is what core/projects.hashKey computes for a private key. Spelled out
// here rather than imported: half of what this test pins is that the digest
// migration 017 writes in SQL is the one a private key really hashes to, and
// deriving both sides from one implementation would prove nothing. The other half
// — that the Go lookup path computes the same digest — is pinned by resolving
// through coreprojects.Repo with the raw key, below.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func migrationsDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("unable to determine source file path")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "..", "schema", "postgres", "migrations")
}

func newProvider(t *testing.T, connStr string) *goose.Provider {
	t.Helper()

	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	provider, err := goose.NewProvider(goose.DialectPostgres, db, os.DirFS(migrationsDir(t)))
	if err != nil {
		t.Fatalf("new goose provider: %v", err)
	}
	return provider
}

// TestApiKeysBackfill runs migration 017 against a database populated the way the
// live one is — projects carrying plaintext key pairs in their own columns — and
// pins the property the whole migration rests on: every key that worked before it
// still resolves through the api_keys table afterwards, with the private one
// stored only as a digest.
//
// 017 drops the columns it reads, so this backfill is the single chance every
// existing key gets to survive. There is no fallback lookup behind it and no
// catch-up pass to fix a miss: a key this statement skips is a key that stops
// authenticating the moment the migration commits.
func TestApiKeysBackfill(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	db := testutil.SetupBarePostgres(t)
	ctx := context.Background()
	provider := newProvider(t, db.ConnStr)

	// Bring the schema up to the state the live database is in today.
	if _, err := provider.UpTo(ctx, versionBeforeApiKeys); err != nil {
		t.Fatalf("migrate to %d: %v", versionBeforeApiKeys, err)
	}

	pool, err := pgxpool.New(ctx, db.ConnStr)
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(ctx,
		`insert into orgs (id, display_name) values ($1, $2)`, "org-backfill00000000", "Backfill Org",
	); err != nil {
		t.Fatalf("insert org: %v", err)
	}

	// Projects as the previous image writes them, in both key formats the live
	// database actually holds: the current "pub_" + crypto/rand hex, and the
	// original "pub_" + xid (base32) that predates it. Both are 24 chars behind the
	// same prefix, so both must survive — an SDK in the wild is holding each.
	projects := []struct {
		id         string
		privateKey string
		publicKey  string
		createTime time.Time
	}{
		{"proj-backfill-000001", "prv_1111111111aaaaaaaaaa", "pub_1111111111aaaaaaaaaa", time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)},
		{"proj-backfill-000002", "prv_2222222222bbbbbbbbbb", "pub_2222222222bbbbbbbbbb", time.Date(2026, 1, 15, 8, 30, 0, 0, time.UTC)},
		{"proj-backfill-xid001", "prv_d9bptmqvq25e9n6g5oag", "pub_d9bptmqvq25e9n6g5ob0", time.Date(2026, 3, 12, 21, 18, 0, 0, time.UTC)},
	}
	for _, p := range projects {
		if _, err := pool.Exec(ctx,
			`insert into projects (id, org_id, display_name, private_api_key, public_api_key, create_time)
			 values ($1, $2, $3, $4, $5, $6)`,
			p.id, "org-backfill00000000", "Project "+p.id, p.privateKey, p.publicKey, p.createTime,
		); err != nil {
			t.Fatalf("insert legacy project %s: %v", p.id, err)
		}
	}

	// Apply 017.
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	queries := dbread.New(pool)
	// The real lookup path, as auth constructs it. It takes the raw key and hashes
	// it internally, which is what makes the subtests below a genuine end-to-end
	// check of the migration's digest against Go's.
	repo := coreprojects.NewRepo(queries, testutil.SetupRedis(t).Client)

	for _, p := range projects {
		t.Run("keys carried over for "+p.id, func(t *testing.T) {
			keys, err := queries.GetApiKeysByProjectID(ctx, p.id)
			if err != nil {
				t.Fatalf("GetApiKeysByProjectID: %v", err)
			}
			if len(keys) != 2 {
				t.Fatalf("got %d keys, want 2 (one per kind)", len(keys))
			}

			byKind := map[string]dbread.ApiKey{}
			for _, k := range keys {
				byKind[k.Kind] = k
			}

			pub, ok := byKind["public"]
			if !ok {
				t.Fatal("no public key row")
			}
			if pub.Token != p.publicKey {
				t.Errorf("public token = %q, want the key itself %q", pub.Token, p.publicKey)
			}
			if want := p.publicKey[:4] + "..." + p.publicKey[len(p.publicKey)-4:]; pub.Masked != want {
				t.Errorf("public masked = %q, want %q", pub.Masked, want)
			}

			prv, ok := byKind["private"]
			if !ok {
				t.Fatal("no private key row")
			}
			if prv.Token == p.privateKey {
				t.Fatal("private key carried over in plaintext")
			}
			if prv.Token != sha256Hex(p.privateKey) {
				t.Errorf("private token = %q, want the key's sha256 hex %q", prv.Token, sha256Hex(p.privateKey))
			}
			if want := p.privateKey[:4] + "..." + p.privateKey[len(p.privateKey)-4:]; prv.Masked != want {
				t.Errorf("private masked = %q, want %q", prv.Masked, want)
			}

			// The keys date from the project, not from when the migration ran.
			for _, k := range keys {
				if !k.CreateTime.Time.Equal(p.createTime) {
					t.Errorf("%s key create_time = %v, want the project's %v", k.Kind, k.CreateTime.Time, p.createTime)
				}
			}
		})

		// The property that actually matters: a key that authenticated before the
		// migration still authenticates after it — presented raw, exactly as an SDK
		// or MCP client sends it, through the same Repo the auth layer calls.
		//
		// Going through Repo rather than the sqlc query is the point. Repo runs
		// core/projects.hashKey on the way in, so a disagreement between that and the
		// digest 017 wrote in SQL fails here. Handing the query a digest this test
		// computed itself would check SQL against SQL and pass whether or not the Go
		// path agrees — and if it doesn't, every existing private key stops
		// authenticating the moment the migration commits, with no fallback behind it.
		t.Run("keys still authenticate for "+p.id, func(t *testing.T) {
			got, err := repo.GetProjectByPrivateApiKey(ctx, p.privateKey)
			if err != nil {
				t.Fatalf("resolve by raw private key: %v", err)
			}
			if strings.TrimSpace(got.ID) != p.id {
				t.Errorf("private key resolved to %q, want %q", got.ID, p.id)
			}

			got, err = repo.GetProjectByPublicApiKey(ctx, p.publicKey)
			if err != nil {
				t.Fatalf("resolve by public key: %v", err)
			}
			if strings.TrimSpace(got.ID) != p.id {
				t.Errorf("public key resolved to %q, want %q", got.ID, p.id)
			}
		})
	}

	// The columns are dropped by the same migration that reads them, so there is no
	// window in which a key resolves through them and nothing to catch up later.
	// Asserting they are gone is asserting that the transitional shape — dual-write,
	// fallback-read, a second migration — was really collapsed and cannot creep back.
	t.Run("the legacy key columns are gone", func(t *testing.T) {
		for _, col := range []string{"private_api_key", "public_api_key"} {
			var exists bool
			if err := pool.QueryRow(ctx,
				`select exists (
				   select 1 from information_schema.columns
				   where table_name = 'projects' and column_name = $1
				 )`, col,
			).Scan(&exists); err != nil {
				t.Fatalf("check projects.%s: %v", col, err)
			}
			if exists {
				t.Errorf("projects.%s survived migration 017", col)
			}
		}
	})
}
