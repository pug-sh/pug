-- +goose Up
create table users (
  create_time timestamptz not null default now(),
  external_id varchar(255) not null,
  id char(20) primary key,
  properties jsonb default '{}'::jsonb,
  custom_properties jsonb default '{}'::jsonb,
  project_id char(20) not null references projects(id) on delete cascade,
  update_time timestamptz not null default now(),
  unique (project_id, external_id)
);

create trigger update_timestamp before
update on users for each row execute procedure moddatetime(update_time);

create index idx_users_properties on users using gin (properties);
create index idx_users_custom_properties on users using gin (custom_properties);

-- +goose Down
drop table if exists users;
