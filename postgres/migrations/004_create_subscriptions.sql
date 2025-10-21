-- +migrate Up
-- Create updated subscriptions table with user association
create table subscriptions (
  create_time timestamptz not null default now(),
  id text primary key,
  last_heartbeat_time timestamptz not null default now(),
  metadata jsonb,
  platform text not null check (platform in ('android', 'ios', 'web')),
  project_id char(20) not null references projects(id) on delete cascade,
  status text not null default 'active' check (status in ('active', 'inactive')),
  token text not null,
  updater text not null default 'system' check (updater in ('system', 'user')),
  update_time timestamptz not null default now(),
  user_id char(20) references users(id) on delete set null
);
create trigger update_timestamp before
update on subscriptions for each row execute procedure moddatetime(update_time);
create index idx_subscriptions_project_status_platform on subscriptions (project_id, status, platform);
create index idx_subscriptions_user_id on subscriptions (user_id);
create index idx_subscriptions_project_user on subscriptions (project_id, user_id);
create index idx_subscriptions_project_user_status on subscriptions (project_id, user_id, status);

-- +migrate Down
drop index if exists idx_subscriptions_project_status_platform;
drop index if exists idx_subscriptions_user_id;
drop index if exists idx_subscriptions_project_user;
drop index if exists idx_subscriptions_project_user_status;
drop table subscriptions;
