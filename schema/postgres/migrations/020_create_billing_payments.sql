-- +goose Up
-- Taking money. The entitlement table beside this one stays the operator's; both
-- tables here are written only by the webhook, which is what makes drift between
-- the quota and the charge structurally impossible rather than a thing to
-- remember.

-- The provider product a negotiated deal is bought against. Operator-written,
-- like every other column on the entitlement row. NULL is every org that is not a
-- deal: the catalog tiers map slug -> product id in config, not here.
alter table billing_entitlements
  add column provider_product_id text
    constraint billing_entitlements_provider_product_check
      check (provider_product_id is null or provider_product_id <> '');

-- The history is a snapshot of the row above, so it takes the column too --
-- otherwise "who pasted this product id, and when" is unanswerable, and that is a
-- support question about the one field that decides whether an org can spend
-- money. The deletion constraint enumerates the value columns, so it is replaced
-- rather than added to.
alter table billing_entitlement_history
  add column provider_product_id text
    constraint billing_entitlement_history_provider_product_check
      check (provider_product_id is null or provider_product_id <> '');

alter table billing_entitlement_history
  drop constraint billing_entitlement_history_deletion_is_empty;

alter table billing_entitlement_history
  add constraint billing_entitlement_history_deletion_is_empty
    check (plan_slug is not null
      or (anchor_day is null and contract_ends_at is null
          and display_name_override is null and included_events_override is null
          and provider_product_id is null and trial_ends_at is null));

-- The provider's subscription, mirrored read-only. Pug owns the quota; this row
-- owns nothing but what the provider said.
create table billing_subscriptions (
  create_time timestamptz not null default now(),
  currency varchar(3) not null
    constraint billing_subscriptions_currency_check check (currency ~ '^[A-Z]{3}$'),
  current_period_end timestamptz,
  current_period_start timestamptz,
  id char(20) primary key,
  org_id char(20) not null references orgs(id) on delete cascade,
  plan_slug varchar(50) not null
    constraint billing_subscriptions_plan_slug_check check (plan_slug <> ''),
  -- Mirror of what the provider charges. Pug never writes it from a person.
  price_cents bigint not null
    constraint billing_subscriptions_price_check check (price_cents >= 0),
  -- No default: a row must name the merchant of record it came from, or a
  -- cutover cannot tell two providers' rows apart.
  provider text not null
    constraint billing_subscriptions_provider_check check (provider <> ''),
  provider_customer_id text not null
    constraint billing_subscriptions_customer_check check (provider_customer_id <> ''),
  provider_status text not null
    constraint billing_subscriptions_provider_status_check check (provider_status <> ''),
  provider_sub_id text not null
    constraint billing_subscriptions_sub_check check (provider_sub_id <> ''),
  -- The CAS column: the delivery timestamp a payload arrived with. An apply is
  -- refused when it is older than what is stored.
  provider_updated_at timestamptz not null,
  -- Pug's vocabulary -- active, past_due, paused, cancelled, expired, failed --
  -- except that a provider state pug has no word for is stored VERBATIM here too.
  -- Not constrained to the six: a value outside them fails to parse at read time
  -- and is therefore not live, which can only ever withhold a plan, never grant
  -- one. Constraining it instead would leave the org on its last known status,
  -- which for a lapsing subscription is the opposite of the safe direction.
  status text not null
    constraint billing_subscriptions_status_check check (status <> ''),
  update_time timestamptz not null default now(),

  constraint billing_subscriptions_provider_sub_key unique (provider, provider_sub_id)
);

-- One LIVE subscription per org, not one row per org: a provider cutover is not
-- atomic, so an org legitimately holds a winding-down row beside a live one. This
-- keeps the invariant that matters -- an org cannot be billed twice -- while
-- permitting the dead row.
create unique index billing_subscriptions_one_live_idx
  on billing_subscriptions (org_id)
  where status in ('active', 'past_due');

create index billing_subscriptions_org_idx on billing_subscriptions (org_id);
create index billing_subscriptions_customer_idx on billing_subscriptions (provider, provider_customer_id);

create trigger update_timestamp before
update on billing_subscriptions for each row execute procedure moddatetime(update_time);

-- Every delivery as sent, so the provider's retries are safe and a payload that
-- failed to apply is replayable. Keyed by (provider, webhook_id) because the id
-- is the provider's namespace and two of them can run side by side.
--
-- payload carries the customer's name, email and billing address -- personal data
-- pug does not otherwise store -- so the controls are on the row: no RPC reads
-- this table, the reconcile pass prunes 90 days after processed_at, and an org
-- erasure deletes its deliveries.
create table billing_webhook_deliveries (
  error text not null default '',
  event_type text not null,
  payload jsonb not null,
  processed_at timestamptz,
  provider text not null
    constraint billing_webhook_deliveries_provider_check check (provider <> ''),
  received_at timestamptz not null default now(),
  webhook_id text not null
    constraint billing_webhook_deliveries_webhook_check check (webhook_id <> ''),

  primary key (provider, webhook_id)
);

-- Drives the prune, which is dated from processed_at and skips the unprocessed.
create index billing_webhook_deliveries_processed_idx
  on billing_webhook_deliveries (processed_at)
  where processed_at is not null;

-- +goose Down
drop table if exists billing_webhook_deliveries;
drop table if exists billing_subscriptions;

alter table billing_entitlement_history
  drop constraint if exists billing_entitlement_history_deletion_is_empty;
alter table billing_entitlement_history drop column if exists provider_product_id;
alter table billing_entitlement_history
  add constraint billing_entitlement_history_deletion_is_empty
    check (plan_slug is not null
      or (anchor_day is null and contract_ends_at is null
          and display_name_override is null and included_events_override is null
          and trial_ends_at is null));

alter table billing_entitlements drop column if exists provider_product_id;
