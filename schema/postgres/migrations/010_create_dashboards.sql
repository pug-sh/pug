-- +goose Up
create table dashboards (
  create_time timestamptz not null default now(),
  description text not null default '',
  display_name varchar(150) not null,
  id char(20) primary key,
  project_id char(20) not null references projects(id) on delete cascade,
  update_time timestamptz not null default now()
);

create index dashboards_project_id_idx on dashboards(project_id);

create trigger update_timestamp before
update on dashboards for each row execute procedure moddatetime(update_time);

create table dashboard_insights (
  create_time timestamptz not null default now(),
  dashboard_id char(20) not null references dashboards(id) on delete cascade,
  description text not null default '',
  display_name varchar(150) not null,
  id char(20) primary key,
  insight_query jsonb not null default '{}'::jsonb,
  layouts jsonb not null default '{}'::jsonb,
  update_time timestamptz not null default now()
);

create index dashboard_insights_dashboard_id_idx on dashboard_insights(dashboard_id);

create trigger update_timestamp before
update on dashboard_insights for each row execute procedure moddatetime(update_time);

-- +goose Down
drop table dashboard_insights;
drop table dashboards;
