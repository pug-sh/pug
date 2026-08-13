-- name: GetUsagePeriod :one
select * from usage_periods where org_id = @org_id and period_start = @period_start;

-- name: GetLatestUsageComputedAt :one
-- Fallback stamp for a period the meter has not reached yet — at a month rollover
-- the current period has no row, which is not the same as never having metered.
select usage_computed_at from usage_periods
where org_id = @org_id order by period_start desc limit 1;

-- name: CountUsageDailyInRange :one
-- Whether the meter has stored anything over a window, across every org. An
-- empty ClickHouse read while this is non-zero is a contradiction an idle
-- deployment cannot produce, which is what lets the pass tell "nothing to count"
-- apart from "counted nothing" -- see docs/architecture/usage.md section 4.
select count(*) from usage_daily where day >= @from_day and day < @to_day;

-- name: ListUsageDailyByOrgID :many
-- Bounded by @row_limit: the request's 400-day cap bounds the span, but a row is
-- (day x project) and projects-per-org is not capped, so the span alone does not
-- bound the result. Ordered oldest-first, so a truncated series loses its newest
-- days rather than an arbitrary slice.
select * from usage_daily
where org_id = @org_id and day >= @from_day and day < @to_day
order by day asc, project_id asc
limit @row_limit;

-- name: ListOrgIDsForUsage :many
select id from orgs order by id asc;

-- name: CountKnownProjectIDs :one
-- How many of the projects a metering pass just read from ClickHouse this
-- Postgres actually knows. UpsertUsageDaily resolves project_id -> org_id in SQL,
-- so an unknown project writes nothing and still reports success; counting stored
-- rows cannot tell that apart, because cells written by earlier correct passes are
-- still there. This asks the question directly -- see docs/architecture/usage.md
-- section 4.
select count(*) from projects where id = any(@project_ids::text[]);
