-- +goose Up
-- SSO and verified domains, phases 1 and 2 (docs/architecture/sso.md).

create table org_domains (
  create_time timestamptz not null default now(),
  -- Lowercase, no trailing dot.
  domain varchar(253) not null
    constraint org_domains_domain_check check (domain = lower(domain)),
  id char(20) primary key,
  org_id char(20) not null references orgs(id) on delete cascade,
  require_sso boolean not null default false,
  -- First SSO sign-in after this claim was verified.
  sso_seen_at timestamptz,
  update_time timestamptz not null default now(),
  verification_method varchar(10),
  verification_token varchar(64) not null,
  verified_at timestamptz,
  constraint org_domains_org_domain_key unique (org_id, domain)
);

-- Several orgs can verify one domain. Sign-in looks up all of them.
create index org_domains_verified_domain_idx
  on org_domains (domain) where verified_at is not null;

create trigger update_timestamp before
update on org_domains for each row execute procedure moddatetime(update_time);

alter table orgs
  -- NULL is auto-join off, the default.
  add column auto_join_role varchar(30)
    constraint orgs_auto_join_role_check check (auto_join_role <> 'ORG_ROLE_ADMIN'),
  add column members_can_create_orgs boolean not null default true;

alter table org_members add column joined_via_domain varchar(253);

-- The domain the session's SSO sign-in proved. Copied on every rotation.
alter table refresh_tokens add column proven_domain varchar(253);

-- +goose Down
alter table refresh_tokens drop column if exists proven_domain;
alter table org_members drop column if exists joined_via_domain;
alter table orgs
  drop column if exists members_can_create_orgs,
  drop column if exists auto_join_role;
drop table if exists org_domains;
