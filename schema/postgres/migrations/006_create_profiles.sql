-- +goose Up
create table profiles (
  create_time timestamptz not null default now(),
  external_id text,
  id char(20) primary key,
  properties jsonb default '{}'::jsonb,
  project_id char(20) not null references projects(id) on delete cascade,
  update_time timestamptz not null default now(),
  unique (id, project_id),
  unique (project_id, external_id)
);

create trigger update_timestamp before
update on profiles for each row execute procedure moddatetime(update_time);

create index idx_profiles_properties on profiles using gin (properties);

-- +goose Down
drop table if exists profiles;
