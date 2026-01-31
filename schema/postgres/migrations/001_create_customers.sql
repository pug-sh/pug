-- +goose Up
create extension if not exists moddatetime;
-- use xid for id
create table customers (
  display_name varchar(150) not null,
  email varchar(255) unique not null,
  id char(20) primary key,
  password_hash varchar(255) not null,
  picture_uri varchar(255) not null,
  create_time timestamptz not null default now(),
  update_time timestamptz not null default now()
);

create trigger update_timestamp before
update on customers for each row execute procedure moddatetime(update_time);

-- +goose Down
drop table customers;
