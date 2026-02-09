-- +goose Up
create table segments (
  create_time timestamptz not null default now(),
  description text,
  filter jsonb not null,
  id char(20) primary key,
  is_active boolean not null default true,
  display_name varchar(255) not null,
  project_id char(20) not null references projects(id) on delete cascade,
  update_time timestamptz not null default now()
);

create trigger update_timestamp before
update on segments for each row execute procedure moddatetime(update_time);

create index idx_segments_is_active on segments (is_active);
create index idx_segments_project_id on segments (project_id);

-- +goose Down
drop table segments;
