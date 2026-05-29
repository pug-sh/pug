-- +goose Up
create table dashboards (
  id char(20) primary key,
  project_id char(20) not null references projects(id) on delete cascade,
  display_name varchar(150) not null,
  description text not null default '',
  -- default_time_range stores common.v1.TimeRangePreset proto enum names;
  -- default_granularity stores shared.insights.v1.Granularity proto enum names.
  default_time_range text not null default 'TIME_RANGE_PRESET_LAST_30_DAYS',
  default_granularity text not null default 'GRANULARITY_DAY',
  create_time timestamptz not null default now(),
  update_time timestamptz not null default now()
);

create index dashboards_project_id_idx on dashboards(project_id);

create trigger update_timestamp before
update on dashboards for each row execute procedure moddatetime(update_time);

-- kind values mirror TileKind in internal/core/dashboards/dashboards.go:
--   1 = TileKindInsight  (insight_query payload)
--   2 = TileKindMarkdown (markdown_body payload)
-- view_mode stores DashboardTileViewMode proto enum names.
-- Tile customization columns extend dashboard_tiles with per-tile presentation
-- options. Storage mirrors the existing dashboard_tiles pattern: text for proto
-- enum names (compare), jsonb for nested / repeated messages
-- (thresholds, header, visualization).
--
-- payload_hash is a sha256 of the deterministic-marshaled DashboardTileInput
-- (id cleared), maintained by the application on every write. Upsert uses it
-- to short-circuit no-op tile UPDATEs in SQL via a `where payload_hash <> $1`
-- predicate, which keeps update_time meaningful (the moddatetime trigger only
-- fires when the row is actually updated). An empty bytea default forces the
-- first write to any existing row through, since no sha256 can match it.
create table dashboard_tiles (
  id            char(20) primary key,
  dashboard_id  char(20) not null references dashboards(id) on delete cascade,
  kind          smallint not null,
  view_mode     text not null default 'DASHBOARD_TILE_VIEW_MODE_UNSPECIFIED',
  display_name  varchar(150) not null default '',
  description   text not null default '',
  insight_query jsonb,
  markdown_body text,
  position      jsonb not null default '{}'::jsonb,
  compare       text  not null default 'COMPARE_PERIOD_UNSPECIFIED',
  thresholds    jsonb not null default '[]'::jsonb,
  header        jsonb not null default '{}'::jsonb,
  visualization jsonb not null default '{}'::jsonb,
  payload_hash  bytea not null default ''::bytea,
  create_time   timestamptz not null default now(),
  update_time   timestamptz not null default now(),
  constraint dashboard_tiles_kind_payload check (
    (kind = 1 and insight_query is not null and markdown_body is null)
    or
    (kind = 2 and markdown_body is not null and insight_query is null)
  ),
  constraint dashboard_tiles_markdown_body_nonempty check (
    markdown_body is null or length(markdown_body) > 0
  )
);

create index dashboard_tiles_dashboard_id_idx on dashboard_tiles(dashboard_id);

create unique index dashboard_tiles_dashboard_id_display_name_idx
  on dashboard_tiles (dashboard_id, lower(display_name))
  where display_name <> '';

create trigger update_timestamp before
update on dashboard_tiles for each row execute procedure moddatetime(update_time);

-- +goose Down
drop table dashboard_tiles;
drop table dashboards;
