create table segments (
  id char(20) primary key,
  project_id char(20) not null references projects(id) on delete cascade,
  name varchar(255) not null,
  description text,
  filter jsonb not null,
  is_active boolean not null default true,
  create_time timestamptz not null default now(),
  update_time timestamptz not null default now()
);

create trigger update_timestamp before
update on segments for each row execute procedure moddatetime(update_time);

create index idx_segments_project_id on segments (project_id);
create index idx_segments_is_active on segments (is_active);
