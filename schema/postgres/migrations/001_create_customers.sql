-- +migrate Up
create extension if not exists moddatetime;
-- use xid for id
create table customers (
  id char(20) primary key,
  display_name varchar(150),
  email varchar(255) unique not null,
  password_hash varchar(255) not null,
  picture_uri varchar(255),
  create_time timestamptz not null default now(),
  update_time timestamptz not null default now()
);
create trigger update_timestamp before
update on customers for each row execute procedure moddatetime(update_time);

-- +migrate Down
drop trigger if exists update_timestamp on customers;
drop table customers;
drop extension if exists moddatetime;
