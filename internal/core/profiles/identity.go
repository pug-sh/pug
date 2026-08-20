package profiles

import (
	chq "github.com/pug-sh/pug/internal/core/clickhouse"
)

// IdentityUnionCTE is the definition of "which distinct_ids are this person"
// for SQL read paths: one (project_id, distinct_id, profile_id) row per
// distinct_id owned by a non-deleted profile — id, external_id, alias ids.
// Join it onto an events scan (IdentityJoinedEvents) and group by
// IdentityUserKeyExpr to stitch a person across the identify() boundary.
//
// arrayDistinct collapses the id = external_id case, so an SDK using one value
// for both cannot produce a duplicate row.
//
// Register via WithIdentityUnion — this reads latest_profiles (twice: the
// ARRAY JOIN and the alias branch's own join) and latest_profile_aliases by
// name. ClickHouse inlines a CTE per reference, so every reference re-runs
// both aggregations; keep references per query to the minimum the shape needs.
//
// The alias branch joins on project_id as well as profile_id. Redundant while
// both CTEs come from WithIdentityUnion (each is already project-scoped), but
// this reads them by name, so the predicate is what keeps the scoping true for
// whatever the caller actually registered.
//
// A cookieless id can never appear here: Identify's anonymous_id is pinned to
// ^$|^anon- and external_id rejects the prefix, so a cookieless event always
// keeps its raw distinct_id as its own key.
//
// claimedIDsCTE deliberately does NOT use this union: its set includes
// soft-deleted tombstones (a tombstoned id must stay invisible, not resurface
// as a derived person), while this union is the *live* identity set.
func IdentityUnionCTE() *chq.Query {
	return chq.NewQuery().
		Select("project_id", "distinct_id", "profile_id").
		From(`(
SELECT p.project_id AS project_id, dist_id AS distinct_id, p.id AS profile_id
FROM latest_profiles p
ARRAY JOIN arrayDistinct(arrayFilter(x -> x != '', [p.id, p.external_id])) AS dist_id
WHERE p.is_deleted = 0
UNION ALL
SELECT pa.project_id AS project_id, pa.alias_id AS distinct_id, pa.profile_id AS profile_id
FROM latest_profile_aliases pa
INNER JOIN latest_profiles p ON p.project_id = pa.project_id AND p.id = pa.profile_id
WHERE p.is_deleted = 0
) u`)
}

// WithIdentityUnion registers the latest_profiles, latest_profile_aliases and
// identity_union CTEs on q in dependency order, returning q for chaining.
func WithIdentityUnion(q *chq.Query, projectID string) *chq.Query {
	return q.
		With("latest_profiles", LatestProfilesCTE(projectID)).
		With("latest_profile_aliases", LatestProfileAliasesCTE(projectID)).
		With("identity_union", IdentityUnionCTE())
}

// IdentityJoinedEvents is the FROM fragment joining an events scan (alias e)
// to the identity_union CTE (alias i). LEFT ANY JOIN picks one identity row
// per event so pathological mappings (a distinct_id matching multiple identity
// rows, e.g. one profile's external_id colliding with another's alias) cannot
// multiply event rows and inflate metrics; which canonical id wins is then
// arbitrary, and two joins in one query may disagree. Build the scan's
// conditions with alias "e" — a bare distinct_id is ambiguous under this join.
func IdentityJoinedEvents() string {
	return "events e LEFT ANY JOIN identity_union i ON i.distinct_id = e.distinct_id"
}

// IdentityUserKeyExpr is the canonical person key for a row of
// IdentityJoinedEvents: the profile id when the event's distinct_id belongs to
// a profile, the raw distinct_id otherwise (an unmatched LEFT ANY JOIN yields
// an empty profile_id, the String default).
func IdentityUserKeyExpr() string {
	return "if(i.profile_id = '', e.distinct_id, i.profile_id)"
}
