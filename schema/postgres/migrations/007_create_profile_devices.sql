-- +goose Up
create table profile_devices (
  create_time timestamptz not null default now(),
  id varchar(36) not null,
  platform text not null check (platform in ('android', 'ios', 'web')),
  profile_id char(20) references profiles(id) on delete set null,
  project_id char(20) not null references projects(id) on delete cascade,
  properties jsonb not null default '{}'::jsonb,
  status text not null default 'active' check (status in ('active', 'inactive')),
  token text,
  update_time timestamptz not null default now(),
  primary key (project_id, id)
);

create trigger update_timestamp before
update on profile_devices for each row execute procedure moddatetime(update_time);

create index idx_profile_devices_profile_id on profile_devices (profile_id);
create index idx_profile_devices_profile_project on profile_devices (profile_id, project_id);
create index idx_profile_devices_project_status_platform on profile_devices (project_id, status, platform);
create index idx_profile_devices_properties on profile_devices using gin (properties);

-- +goose Down
drop table profile_devices;
