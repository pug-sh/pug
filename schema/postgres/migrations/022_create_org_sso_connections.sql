-- +goose Up
-- SSO connections, phase 3 (docs/architecture/sso.md).

create table org_sso_connections (
  client_id varchar(255) not null,
  -- AES-GCM under PUG_SSO_SECRET_KEY.
  client_secret_ciphertext bytea not null,
  create_time timestamptz not null default now(),
  id char(20) primary key,
  issuer_url varchar(2048) not null,
  label varchar(64) not null,
  org_id char(20) not null references orgs(id) on delete cascade,
  update_time timestamptz not null default now(),
  -- Target of org_domains' foreign key, which keeps a connection inside its org.
  constraint org_sso_connections_org_id_id_key unique (org_id, id)
);

create trigger update_timestamp before
update on org_sso_connections for each row execute procedure moddatetime(update_time);

alter table org_domains
  add column sso_connection_id char(20),
  add constraint org_domains_sso_connection_fkey
    foreign key (org_id, sso_connection_id)
    references org_sso_connections (org_id, id)
    on delete set null (sso_connection_id),
  add constraint org_domains_sso_connection_id_check
    check (sso_connection_id is null or verified_at is not null);

-- One connection per domain, across all orgs.
create unique index org_domains_sso_connection_domain_key
  on org_domains (domain) where sso_connection_id is not null;

-- +goose Down
drop index if exists org_domains_sso_connection_domain_key;
alter table org_domains
  drop constraint if exists org_domains_sso_connection_id_check,
  drop constraint if exists org_domains_sso_connection_fkey,
  drop column if exists sso_connection_id;
drop table if exists org_sso_connections;
