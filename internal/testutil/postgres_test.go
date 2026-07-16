package testutil

import (
	"net/url"
	"strconv"
	"testing"
)

const testBaseDSN = "postgres://postgres:postgres@localhost:32768/pug_template?sslmode=disable"

// TestPoolDSNCarriesConnectionCap pins the quiet half of the plainDSN/poolDSN
// split. A pool built from a DSN that has lost pool_max_conns works perfectly and
// silently takes pgxpool's max(4, NumCPU) default, so no other test in the suite
// would notice the cap going missing.
//
// The loud half needs no test: a pool_max_conns reaching migrate's plain pgx
// connection is forwarded to the server as a runtime parameter and fails every
// Postgres test in the repo outright.
func TestPoolDSNCarriesConnectionCap(t *testing.T) {
	got := mustParse(t, poolDSN(mustParse(t, testBaseDSN), "test_db"))

	if want := strconv.Itoa(pgPoolMaxConns); got.Query().Get("pool_max_conns") != want {
		t.Errorf("pool_max_conns = %q, want %q", got.Query().Get("pool_max_conns"), want)
	}
	if got.Path != "/test_db" {
		t.Errorf("path = %q, want %q", got.Path, "/test_db")
	}
	if got.Query().Get("sslmode") != "disable" {
		t.Error("poolDSN dropped the base DSN's own parameters")
	}
}

func TestPlainDSNOmitsPoolParams(t *testing.T) {
	got := mustParse(t, plainDSN(mustParse(t, testBaseDSN), "test_db"))

	if got.Query().Has("pool_max_conns") {
		t.Error("plainDSN carries pool_max_conns, which pgx forwards to the server as a runtime parameter, failing the connection")
	}
	if got.Path != "/test_db" {
		t.Errorf("path = %q, want %q", got.Path, "/test_db")
	}
}

// TestDSNBuildersDoNotMutateBase guards the copy in each builder. sharedPostgres
// hands the same *url.URL to every test in the package, so a builder that wrote
// through the pointer instead of its own copy would rewrite the target database
// for everything that came after it.
func TestDSNBuildersDoNotMutateBase(t *testing.T) {
	base := mustParse(t, testBaseDSN)

	plainDSN(base, "one")
	poolDSN(base, "two")

	if got := base.String(); got != testBaseDSN {
		t.Errorf("base = %q, want it untouched at %q", got, testBaseDSN)
	}
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}
