package attribution

import (
	"os"
	"regexp"
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
// TestIntegrationWebAnalytics/mutation_008_matches_attribution_derive is the
// authority on whether ClickHouse agrees; it needs Docker and a corpus row per
// case. This is the cheap guard that runs in `go test ./...` on every change to
// either side, and it is the one that fires when googleNonSearchLabels grows a
// label the SQL does not.
func TestGoogleHostSQLMirror(t *testing.T) {
	data, err := os.ReadFile("../../schema/clickhouse/migrations/008_add_web_analytics_columns.sql")
	if err != nil {
		t.Fatalf("read migration 008: %v", err)
	}
	sql := string(data)

	include := extractSQLRegexp(t, sql, `match\(ref, '(\(\^\|\\\\\.\)google[^']*)'\)`)
	exclude := extractSQLRegexp(t, sql, `match\(ref, '(\(\^\|\\\\\.\)\(accounts[^']*)'\)`)

	// Every host TestIsGoogleHost and TestIsGoogleHostNonSearchProducts assert
	// on, plus the shapes that separate the two implementations: a non-search
	// label that is not adjacent to google.<tld>, a four-label host whose tail
	// is not TLD-shaped, and the reverse-DNS app host.
	hosts := []string{
		"google.com", "www.google.com", "google.co.uk", "google.de",
		"images.google.co.in", "news.google.com", "www.google.co.uk",
		"accounts.google.com", "mail.google.com", "search.google.com",
		"accounts.google.co.uk", "mail.google.de", "eu.mail.google.com",
		"mail.example.google.com", "accounts.example.com.google.com",
		"mail.google.com.example.co", "com.google.android.gm", "google.android.gm",
		"googleusercontent.com", "notgoogle.com", "agoogle.de", "mailgoogle.com",
		"searchgoogle.co.uk", "google.museum", "example.com", "",
	}
	for _, h := range hosts {
		// The bare "google" utm_source is carried by the SQL's separate
		// `ref = 'google'` / `src IN ('google', …)` terms, not by these regexes.
		if h == "google" {
			continue
		}
		want := isGoogleHost(h)
		got := include.MatchString(h) && !exclude.MatchString(h)
		if got != want {
			t.Errorf("host %q: migration 008 says search=%v, isGoogleHost says %v", h, got, want)
		}
	}
}

// extractSQLRegexp pulls one regex literal out of the migration and undoes the
// SQL string escaping ('\\.' in the file is the regex '\.').
func extractSQLRegexp(t *testing.T, sql, pattern string) *regexp.Regexp {
	t.Helper()
	m := regexp.MustCompile(pattern).FindStringSubmatch(sql)
	if m == nil {
		t.Fatalf("no regex literal in migration 008 matching %s — if the google branch was rewritten, this test must be rewritten with it, not deleted", pattern)
	}
	return regexp.MustCompile(strings.ReplaceAll(m[1], `\\`, `\`))
}
