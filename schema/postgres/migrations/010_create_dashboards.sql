-- +goose Up
create table dashboards (
  id char(20) primary key,
  project_id char(20) not null references projects(id) on delete cascade,
  display_name varchar(150) not null,
  description text not null default '',
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
-- default_time_range stores common.v1.TimeRangePreset proto enum names.
create table dashboard_tiles (
  id            char(20) primary key,
  dashboard_id  char(20) not null references dashboards(id) on delete cascade,
  kind          smallint not null,
  view_mode     text not null default 'DASHBOARD_TILE_VIEW_MODE_UNSPECIFIED',
  default_time_range text not null default 'TIME_RANGE_PRESET_UNSPECIFIED',
  display_name  varchar(150) not null default '',
  description   text not null default '',
  insight_query jsonb,
  markdown_body text,
  layouts       jsonb not null default '{}'::jsonb,
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
