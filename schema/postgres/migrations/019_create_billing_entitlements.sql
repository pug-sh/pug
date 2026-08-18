-- +goose Up
-- What an org is allowed to send (docs/architecture/billing.md). Signup writes
-- nothing here: no row, or a NULL column, derives from orgs.create_time or from
-- the plan. No price column -- the payments provider owns the amount.

create table billing_entitlements (
  anchor_day smallint
    constraint billing_entitlements_anchor_day_check
      check (anchor_day between 1 and 31),
  -- The end of the deal, not of a quota window.
  contract_ends_at timestamptz,
  create_time timestamptz not null default now(),
  display_name_override varchar(150),
  included_events_override bigint
    constraint billing_entitlements_override_check
      check (included_events_override > 0),
  note text not null default '',
  org_id char(20) primary key references orgs(id) on delete cascade,
  -- Not constrained to a slug list: the catalog is Go, and a reprice mints a slug.
  plan_slug varchar(50) not null
    constraint billing_entitlements_plan_slug_check check (plan_slug <> ''),
  trial_ends_at timestamptz,
  update_time timestamptz not null default now(),
  -- A custom plan has no catalog quota to fall back on.
  constraint billing_entitlements_custom_needs_quota
    check (plan_slug <> 'custom' or included_events_override is not null)
);

create trigger update_timestamp before
update on billing_entitlements for each row execute procedure moddatetime(update_time);

-- Append-only snapshots of the row above: what was true then, and who changed it.
-- No FK to orgs -- the org being gone is when this gets asked. A NULL plan_slug,
-- impossible on a live row, records a deletion.
create table billing_entitlement_history (
  actor varchar(150) not null
    constraint billing_entitlement_history_actor_check check (actor <> ''),
  anchor_day smallint,
  changed_at timestamptz not null default now(),
  contract_ends_at timestamptz,
  display_name_override varchar(150),
  id char(20) primary key,
  included_events_override bigint,
  note text not null default '',
  org_id char(20) not null,
  plan_slug varchar(50)
    constraint billing_entitlement_history_plan_slug_check check (plan_slug <> ''),
  trial_ends_at timestamptz
);

create index billing_entitlement_history_org_idx
  on billing_entitlement_history (org_id, changed_at desc);

-- +goose Down
drop table if exists billing_entitlement_history;
drop table if exists billing_entitlements;
