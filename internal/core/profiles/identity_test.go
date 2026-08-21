package profiles_test

import (
	"strings"
	"testing"

	chq "github.com/pug-sh/pug/internal/core/clickhouse"
	"github.com/pug-sh/pug/internal/core/profiles"
)

// The identity CTEs are inlined once per reference, so reading either source
// twice doubles the profiles scan on every resolving insight. Pinned here
// rather than in the insights package so the assertion sits next to the SQL.
func TestIdentityUnionCTE_ReadsEachSourceOnce(t *testing.T) {
	sql, _, err := profiles.WithIdentityUnion(chq.NewQuery().Select("distinct_id").From("identity_union"), "proj_123").Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	for _, want := range []string{
		"latest_profiles AS (",
		"latest_profile_aliases AS (",
		"identity_union AS (",
		"ARRAY JOIN dist_ids AS dist_id",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("expected %q in SQL, got: %s", want, sql)
		}
	}
	if got := strings.Count(sql, "FROM profiles"); got != 1 {
		t.Errorf("expected 1 profiles read, got %d: %s", got, sql)
	}
	if got := strings.Count(sql, "FROM profile_aliases"); got != 1 {
		t.Errorf("expected 1 profile_aliases read, got %d: %s", got, sql)
	}
	if got := strings.Count(sql, "latest_profiles p"); got != 1 {
		t.Errorf("expected 1 latest_profiles reference, got %d: %s", got, sql)
	}
	if got := strings.Count(sql, "FROM latest_profile_aliases"); got != 1 {
		t.Errorf("expected 1 latest_profile_aliases reference, got %d: %s", got, sql)
	}
}

// arrayDistinct over the concatenated sources is what keeps a value repeated
// between id, external_id and an alias_id from emitting two rows — an INNER
// JOIN consumer (profileActivitySummaryCTE) would merge its state twice.
func TestIdentityUnionCTE_DedupsAcrossIDSources(t *testing.T) {
	sql, _, err := profiles.IdentityUnionCTE().Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(sql, "arrayDistinct(arrayFilter(x -> x != '', arrayConcat([p.id, p.external_id], a.alias_ids)))") {
		t.Errorf("expected all three id sources deduped in one arrayDistinct, got: %s", sql)
	}
}

// The join must be tenant-scoped: identity_union is referenced by name, so a
// caller registering a differently-scoped CTE would otherwise resolve a
// distinct_id against another project's profiles.
func TestIdentityJoinedEvents_ScopesByProject(t *testing.T) {
	if got := profiles.IdentityJoinedEvents(); !strings.Contains(got, "i.project_id = e.project_id") {
		t.Errorf("expected project_id in the join predicate, got: %s", got)
	}
}
