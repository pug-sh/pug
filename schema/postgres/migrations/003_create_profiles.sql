-- +goose Up
create table profiles (
  auto_properties jsonb default '{}'::jsonb,
  create_time timestamptz not null default now(),
  custom_properties jsonb default '{}'::jsonb,
  external_id text,
  id char(20) primary key,
  project_id char(20) not null references projects(id) on delete cascade,
  update_time timestamptz not null default now(),
  unique (id, project_id),
  unique (project_id, external_id)
);

create trigger update_timestamp before
update on profiles for each row execute procedure moddatetime(update_time);

create index idx_profiles_auto_properties on profiles using gin (auto_properties);
create index idx_profiles_custom_properties on profiles using gin (custom_properties);

-- +goose Down
drop table if exists profiles;
