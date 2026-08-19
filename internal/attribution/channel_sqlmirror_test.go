package attribution

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestGoogleHostSQLMirror pins the google branch of migration 008's SQL mirror
// against isGoogleHost WITHOUT a ClickHouse. The regexes are read out of the
// migration rather than restated here, so this cannot pass by both copies
// drifting together, and ClickHouse's match() is RE2 — the same engine Go's
// regexp is — so the comparison is of the shipped predicate, not of a
// paraphrase of it.
//
// Three things have to hold, and each closes a way the two sides could drift
// while the other two checks stayed green:
//
//  1. BOTH predicate arms are checked. The migration writes the rule twice, for
//     src and for ref; reading one arm makes a src-only edit invisible.
//  2. The exclusion alternation must equal googleNonSearchLabels EXACTLY. A
//     label added on one side only is otherwise unreachable — a host list
//     written out by hand here contains no case for a label nobody has added
//     yet, so the set comparison is what catches a growing set, in both
//     directions.
//  3. The host corpus is DERIVED from googleNonSearchLabels, so a new label
//     brings its own cases (bare, ccTLD, nested, non-adjacent) with it.
//
// TestIntegrationWebAnalytics/mutation_008_matches_attribution_derive is the
// authority on whether ClickHouse agrees; it needs Docker and a corpus row per
// case. This is the cheap guard that runs in `go test ./...` on every change to
// either side.
func TestGoogleHostSQLMirror(t *testing.T) {
	data, err := os.ReadFile("../../schema/clickhouse/migrations/008_add_web_analytics_columns.sql")
	if err != nil {
		t.Fatalf("read migration 008: %v", err)
	}
	sql := string(data)

	// Both arms, by the column each is written against.
	for _, arm := range []string{"src", "ref"} {
		include := extractSQLRegexp(t, sql, arm, `'(\(\^\|\\\\\.\)google[^']*)'`)
		exclude := extractSQLRegexp(t, sql, arm, `'(\(\^\|\\\\\.\)\([a-z|]+\)\\\\\.google[^']*)'`)

		// (2) the alternation IS the label set, not merely a superset of the
		// labels this file happens to name.
		if got := alternationLabels(t, exclude.String()); !slices.Equal(got, sortedLabels()) {
			t.Errorf("migration 008 %s arm excludes %v, googleNonSearchLabels is %v", arm, got, sortedLabels())
		}

		for _, h := range mirrorHosts() {
			// The bare "google" utm_source is carried by the SQL's separate
			// `ref = 'google'` / `src IN ('google', …)` terms, not by these.
			if h == "google" {
				continue
			}
			want := isGoogleHost(h)
			got := include.MatchString(h) && !exclude.MatchString(h)
			if got != want {
				t.Errorf("%s arm, host %q: migration 008 says search=%v, isGoogleHost says %v", arm, h, got, want)
			}
		}
	}
}

// mirrorHosts derives a corpus from googleNonSearchLabels — so a label added to
// that set brings its own cases — plus the fixed shapes that separate the two
// implementations regardless of which labels exist.
func mirrorHosts() []string {
	hosts := []string{
		"google.com", "www.google.com", "google.co.uk", "google.de",
		"images.google.co.in", "news.google.com", "www.google.co.uk",
		"mail.google.com.example.co", "com.google.android.gm", "google.android.gm",
		"googleusercontent.com", "notgoogle.com", "agoogle.de", "example.com",
		"google.museum", "",
	}
	for _, l := range googleNonSearchLabels {
		hosts = append(hosts,
			l+".google.com",             // the plain case
			l+".google.co.uk",           // per-ccTLD, not per-host
			"eu."+l+".google.com",       // adjacency decides, not position
			l+".example.google.com",     // same label, NOT adjacent: still Search
			l+".example.com.google.com", // ditto, further away
			l+"google.com",              // no label boundary: not an exception
		)
	}
	return hosts
}

func sortedLabels() []string {
	out := slices.Clone(googleNonSearchLabels)
	slices.Sort(out)
	return out
}

// alternationLabels pulls "accounts|mail|search" out of the exclusion regex and
// returns it sorted, so the comparison is of sets rather than of spelling.
func alternationLabels(t *testing.T, re string) []string {
	t.Helper()
	m := regexp.MustCompile(`\(([a-z|]+)\)\\\.google\\\.`).FindStringSubmatch(re)
	if m == nil {
		t.Fatalf("no label alternation in exclusion regex %q", re)
	}
	out := strings.Split(m[1], "|")
	slices.Sort(out)
	return out
}

// extractSQLRegexp pulls one regex literal out of the given match(<arm>, '…')
// call in the migration and undoes the SQL string escaping ('\\.' in the file
// is the regex '\.').
func extractSQLRegexp(t *testing.T, sql, arm, pattern string) *regexp.Regexp {
	t.Helper()
	full := fmt.Sprintf(`match\(%s, %s\)`, arm, pattern)
	m := regexp.MustCompile(full).FindStringSubmatch(sql)
	if m == nil {
		t.Fatalf("no regex literal in migration 008 matching %s — if the google branch was rewritten, this test must be rewritten with it, not deleted", full)
	}
	return regexp.MustCompile(strings.ReplaceAll(m[1], `\\`, `\`))
}
