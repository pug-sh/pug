-- +goose Up
create table projects (
  create_time timestamptz not null default now(),
  created_by char(20) not null references customers(id),
  display_name varchar(150) not null,
  fcm_service_json text,
  id char(20) primary key,
  org_id char(20) not null references orgs(id) on delete cascade,
  private_api_key char(24) not null unique,
  public_api_key char(24) not null unique,
  update_time timestamptz not null default now(),
  unique (org_id, display_name)
);

create trigger update_timestamp before
update on projects for each row execute procedure moddatetime(update_time);


-- +goose Down
drop table projects;
