-- +goose Up
create table projects (
  create_time timestamptz not null default now(),
  customer_id char(20) not null references customers(id) on delete cascade,
  display_name varchar(150) not null,
  fcm_service_json text,
  id char(20) primary key,
  private_api_key char(24) not null unique,
  public_api_key char(24) not null unique,
  update_time timestamptz not null default now(),
  unique (customer_id, display_name)
);

create trigger update_timestamp before
update on projects for each row execute procedure moddatetime(update_time);

create index idx_projects_private_api_key on projects (private_api_key);
create index idx_projects_public_api_key on projects (public_api_key);

-- +goose Down
drop table projects;
