-- name: GetUsagePeriod :one
select * from usage_periods where org_id = @org_id and period_start = @period_start;

-- name: GetLatestUsageComputedAt :one
-- Fallback stamp for a period the meter has not reached yet — at a month rollover
-- the current period has no row, which is not the same as never having metered.
select usage_computed_at from usage_periods
where org_id = @org_id order by period_start desc limit 1;

-- name: ListUsageDailyByOrgID :many
select * from usage_daily
where org_id = @org_id and day >= @from_day and day < @to_day
order by day asc, project_id asc;

-- name: ListOrgIDsForUsage :many
select id from orgs order by id asc;
