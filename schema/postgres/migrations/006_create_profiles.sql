-- +goose Up
create table profiles (
  create_time timestamptz not null default now(),
  deletion_time timestamptz,
  external_id text,
  id char(20) primary key,
  properties jsonb default '{}'::jsonb,
  project_id char(20) not null references projects(id) on delete cascade,
  update_time timestamptz not null default now(),
  unique (id, project_id)
);

-- Only one active (non-deleted) profile per (project_id, external_id).
-- Soft-deleted rows don't block re-identification with the same external_id.
create unique index profiles_project_id_external_id_active
  on profiles (project_id, external_id) where deletion_time is null;

create trigger update_timestamp before
update on profiles for each row execute procedure moddatetime(update_time);

create index idx_profiles_properties on profiles using gin (properties);
create index idx_profiles_project_create_time on profiles (project_id, create_time desc, id desc);

-- +goose Down
drop table if exists profiles;
