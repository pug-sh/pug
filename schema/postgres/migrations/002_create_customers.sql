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

-- External identity providers (Google, GitHub, Microsoft, …). provider values
-- are enforced in Go (ProviderName); no DB check constraint — intentional.
create table customer_identities (
  id char(20) primary key,
  customer_id char(20) not null references customers(id) on delete cascade,
  provider text not null,
  provider_subject text not null,
  create_time timestamptz not null default now(),
  unique (provider, provider_subject)
);

create index customer_identities_customer_id_idx on customer_identities (customer_id);

create trigger update_timestamp before
update on customers for each row execute procedure moddatetime(update_time);

-- +goose Down
drop table customer_identities;
drop table customers;
