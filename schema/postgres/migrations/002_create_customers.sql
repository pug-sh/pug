-- +goose Up
create table customers (
  create_time timestamptz not null default now(),
  display_name varchar(150) not null,
  email varchar(255) not null,
  id char(20) primary key,
  password_hash varchar(255) not null,
  picture_uri varchar(255) not null,
  update_time timestamptz not null default now()
);

-- Functional unique index makes the email column case-insensitive for
-- uniqueness — Bob@example.com and bob@example.com cannot both insert.
-- Reads in service.go also use lower(email) = lower(@email) so storage
-- case is preserved while comparisons are case-insensitive.
create unique index customers_email_lower_idx on customers (lower(email));

create trigger update_timestamp before
update on customers for each row execute procedure moddatetime(update_time);

-- +goose Down
drop table customers;
