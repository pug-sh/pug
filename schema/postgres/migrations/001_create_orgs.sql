-- +goose Up
create extension if not exists moddatetime;

create table orgs (
  create_time timestamptz not null default now(),
  display_name varchar(150) not null,
  id char(20) primary key,
  update_time timestamptz not null default now()
);

create trigger update_timestamp before
update on orgs for each row execute procedure moddatetime(update_time);


-- +goose Down
drop table orgs;
