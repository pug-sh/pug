-- +goose Up
create table projects (
  api_key char(20) not null unique,
  create_time timestamptz not null default now(),
  customer_id char(20) not null references customers(id) on delete cascade,
  display_name varchar(150) not null,
  fcm_service_json text,
  id char(20) primary key,
  update_time timestamptz not null default now(),
  unique (customer_id, display_name)
);

create trigger update_timestamp before
update on projects for each row execute procedure moddatetime(update_time);

create index idx_projects_api_key on projects (api_key);

-- +goose Down
drop table projects;
