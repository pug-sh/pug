-- +goose Up
-- External identity providers (Google, GitHub, Microsoft, …). provider values
-- are enforced in Go (ProviderName); no DB check constraint — intentional.
-- The unique constraint is named explicitly so Go code (oauth.resolve) can
-- reference a chosen name rather than a Postgres-derived one.
create table customer_identities (
  id char(20) primary key,
  customer_id char(20) not null references customers(id) on delete cascade,
  provider text not null,
  provider_subject text not null,
  create_time timestamptz not null default now(),
  constraint customer_identities_provider_subject_key unique (provider, provider_subject)
);

create index customer_identities_customer_id_idx on customer_identities (customer_id);

-- +goose Down
drop table customer_identities;
