-- Write your migrate up statements here
create extension if not exists moddatetime;
-- use xid for id
create table customers (
  display_name varchar(150) not null,
  email varchar(255) unique not null,
  id char(20) primary key,
  picture_uri varchar(255) not null,
  create_time timestamptz not null default now(),
  update_time timestamptz not null default now()
);
create trigger update_timestamp before
update on customers for each row execute procedure moddatetime(update_time);
---- create above / drop below ----
drop trigger if exists update_timestamp on clients;
drop table customers;
drop extension if exists moddatetime;
