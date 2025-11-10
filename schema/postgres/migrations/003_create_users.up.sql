create table users (
  create_time timestamptz not null default now(),
  external_id varchar(255) not null,
  id char(20) primary key,
  metadata jsonb,
  project_id char(20) not null references projects(id) on delete cascade,
  update_time timestamptz not null default now(),
  unique (project_id, external_id)
);
create trigger update_timestamp before
update on users for each row execute procedure moddatetime(update_time);
create index idx_users_project_id on users (project_id);
create index idx_users_external_id on users (external_id);
create index idx_users_project_external on users (project_id, external_id);
