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
// Register via WithIdentityUnion, which supplies the two CTEs read by name.
// ClickHouse inlines a CTE per reference, so every reference re-runs both
// aggregations; keep references per query to the minimum the shape needs.
//
// The alias join carries project_id because this reads the CTEs by name — the
// predicate is what scopes whatever the caller actually registered.
//
// A cookieless id can never appear here: Identify's anonymous_id is pinned to
// ^$|^anon- and external_id rejects the prefix.
//
// claimedIDsCTE deliberately does NOT use this union: its set includes
// soft-deleted tombstones (a tombstoned id must stay invisible), while this
// union is the *live* identity set.
func IdentityUnionCTE() *chq.Query {
	return chq.NewQuery().
		Select("project_id", "dist_id AS distinct_id", "profile_id").
		From(`(
SELECT p.project_id AS project_id, p.id AS profile_id,
arrayDistinct(arrayFilter(x -> x != '', arrayConcat([p.id, p.external_id], a.alias_ids))) AS dist_ids
FROM latest_profiles p
LEFT JOIN (
SELECT project_id, profile_id, groupArray(alias_id) AS alias_ids
FROM latest_profile_aliases
GROUP BY project_id, profile_id
) a ON a.project_id = p.project_id AND a.profile_id = p.id
WHERE p.is_deleted = 0
) ARRAY JOIN dist_ids AS dist_id`)
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
// conditions with alias "e": ClickHouse resolves a bare distinct_id to the
// left table silently, so a missed alias is right by luck, not caught.
//
// The join carries project_id for the same reason the CTE's own alias join
// does — identity_union is registered by name, so the predicate is what keeps
// resolution inside one tenant.
func IdentityJoinedEvents() string {
	return "events e LEFT ANY JOIN identity_union i ON i.project_id = e.project_id AND i.distinct_id = e.distinct_id"
}

// IdentityUserKeyExpr is the canonical person key for a row of
// IdentityJoinedEvents: the profile id when the event's distinct_id belongs to
// a profile, the raw distinct_id otherwise (an unmatched LEFT ANY JOIN yields
// an empty profile_id, the String default).
func IdentityUserKeyExpr() string {
	return "if(i.profile_id = '', e.distinct_id, i.profile_id)"
}
