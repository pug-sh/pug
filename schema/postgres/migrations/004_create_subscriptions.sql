-- +goose Up
create table subscriptions (
  create_time timestamptz not null default now(),
  id text primary key,
  last_heartbeat_time timestamptz not null default now(),
  metadata jsonb,
  platform text not null check (platform in ('android', 'ios', 'web')),
  profile_id char(20) references profiles(id) on delete set null,
  project_id char(20) not null references projects(id) on delete cascade,
  status text not null default 'active' check (status in ('active', 'inactive')),
  token text not null,
  update_time timestamptz not null default now(),
  updater text not null default 'system' check (updater in ('system', 'user'))
);

create trigger update_timestamp before
update on subscriptions for each row execute procedure moddatetime(update_time);

create index idx_subscriptions_profile_id on subscriptions (profile_id);
create index idx_subscriptions_project_status_platform on subscriptions (project_id, status, platform);

-- +goose Down
drop table subscriptions;
