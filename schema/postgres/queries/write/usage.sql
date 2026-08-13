-- name: UpsertUsageDaily :batchexec
-- The org comes from the project row, so a cell whose project is gone from
-- Postgres inserts nothing. That is routine, not a race: deleting a project does
-- not delete its ClickHouse events, so the meter re-reads and re-drops them.
-- Gated on a changed count so a finished day inside the rescan window is not
-- rewritten every tick.
insert into usage_daily (day, event_count, org_id, project_id)
select @day, @event_count, p.org_id, @project_id
from projects p where p.id = @project_id
on conflict (project_id, day) do update
set event_count = excluded.event_count,
    org_id = excluded.org_id,
    update_time = now()
where usage_daily.event_count is distinct from excluded.event_count;

-- name: RefreshUsagePeriod :one
-- Summed on the write pool because the daily rows were written there moments
-- earlier. Deliberately NOT gated on a changed count, unlike UpsertUsageDaily:
-- usage_computed_at has to advance every tick, idle org or not.
insert into usage_periods (event_count, org_id, period_end, period_start, usage_computed_at)
select coalesce(sum(d.event_count), 0)::bigint, @org_id, @period_end, @period_start, now()
from usage_daily d
where d.org_id = @org_id and d.day >= @from_day and d.day < @to_day
on conflict (org_id, period_start) do update
set event_count = excluded.event_count,
    period_end = excluded.period_end,
    usage_computed_at = now()
returning event_count;

-- name: DeleteUnmeteredUsageDaily :execrows
-- Cells the recompute no longer sees at all: their events were deleted (GDPR
-- erasure, a dropped partition). Upserting only what ClickHouse returned would
-- leave the old count standing forever.
delete from usage_daily d
where d.day >= @from_day and d.day < @to_day
  and not exists (
    select 1
    from unnest(@project_ids::text[]) with ordinality as p(project_id, n)
    join unnest(@days::date[]) with ordinality as k(day, n) on p.n = k.n
    where p.project_id = d.project_id and k.day = d.day
  );

-- name: PruneUsageDaily :execrows
delete from usage_daily where day < @older_than;
