-- +goose Up
-- Event usage metering (docs/architecture/usage.md). Counts are reporting, not
-- enforcement: the meter reads ClickHouse after the fact, so nothing here can
-- reject, throttle or delay an event.

-- Day rows sum into any period shape without recomputation, and the grain is what
-- the dashboard charts. org_id is denormalized so a period sum stays single-table.
create table usage_daily (
  day date not null,
  event_count bigint not null default 0,
  org_id char(20) not null references orgs(id) on delete cascade,
  project_id char(20) not null references projects(id) on delete cascade,
  update_time timestamptz not null default now(),
  primary key (project_id, day)
);

create index usage_daily_org_day_idx on usage_daily (org_id, day);

-- Both day-range deletes -- the per-pass reconcile and the daily prune -- filter
-- on `day` alone, and neither index above leads with it. Without this they
-- seq-scan the whole table; the reconcile pays that on every single pass.
create index usage_daily_day_idx on usage_daily (day);

-- Pre-summed so the dashboard's per-page-load read is a single-row lookup.
create table usage_periods (
  event_count bigint not null default 0,
  org_id char(20) not null references orgs(id) on delete cascade,
  period_end timestamptz not null,
  period_start timestamptz not null,
  -- No update_time twin: nothing re-sums this row without recomputing usage, so
  -- this is both the freshness stamp and the modification time.
  usage_computed_at timestamptz not null default now(),
  primary key (org_id, period_start)
);

-- When each cron task last ran, so a job firing every N minutes can still hold
-- some of its work to daily. In a process's memory this would reset on every
-- run, since each cron pass is a fresh process. Keys are namespaced by job
-- ("usage.prune"), which cron.State does for the caller.
--
-- Control state, NOT run history: written only on success, read only to gate
-- work. Whether a run happened at all is a question for the CronJob's own Job
-- objects and for usage_periods.usage_computed_at going stale.
create table cron_state (
  task varchar(80) primary key,
  last_run_at timestamptz not null
);

-- +goose Down
drop table if exists cron_state;
drop table if exists usage_periods;
drop table if exists usage_daily;
