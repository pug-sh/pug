-- +goose Up
-- Taking money. billing_subscriptions has three writers -- the webhook, the
-- reconcile pass and ConfirmCheckout -- and all three go through the one CAS below,
-- so no second notion of "newer" exists.

-- The provider product a negotiated deal is bought against, operator-written like
-- the rest of the row. NULL is every org that is not a deal: catalog tiers map
-- slug -> product id in config, not here.
alter table billing_entitlements
  add column provider_product_id text
    constraint billing_entitlements_provider_product_check
      check (provider_product_id is null or provider_product_id <> '');

-- The history is a snapshot of the row above, so it takes the column too. The
-- deletion constraint enumerates the value columns, so it is replaced rather than
-- added to.
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
          and provider_product_id is null and retention_days_override is null
          and trial_ends_at is null));

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
  -- The CAS column: the delivery timestamp a payload arrived with, so ordering here
  -- is arrival order. An apply older than what is stored is refused; the reconcile
  -- pass corrects a delivery that arrived out of order.
  provider_updated_at timestamptz not null,
  -- Pug's vocabulary -- active, past_due, paused, cancelled, expired, failed -- and
  -- a provider state pug has no word for, stored VERBATIM. Not constrained to the
  -- six: an unparsable value is not live, which can only withhold a plan.
  status text not null
    constraint billing_subscriptions_status_check check (status <> ''),
  update_time timestamptz not null default now(),

  constraint billing_subscriptions_provider_sub_key unique (provider, provider_sub_id)
);

-- One LIVE subscription per org, not one row per org: a provider cutover is not
-- atomic, so a winding-down row may sit beside a live one. An org still cannot be
-- billed twice, which is the invariant that matters.
create unique index billing_subscriptions_one_live_idx
  on billing_subscriptions (org_id)
  where status in ('active', 'past_due');

create index billing_subscriptions_org_idx on billing_subscriptions (org_id);
create index billing_subscriptions_customer_idx on billing_subscriptions (provider, provider_customer_id);

create trigger update_timestamp before
update on billing_subscriptions for each row execute procedure moddatetime(update_time);

-- The checkouts pug itself started. Attribution prefers a ref from here over the
-- org_id in a payload's metadata: static payment links let the BUYER set metadata,
-- so an org id there names an org rather than proving one.
create table billing_checkout_sessions (
  create_time timestamptz not null default now(),
  org_id char(20) not null references orgs(id) on delete cascade,
  provider text not null
    constraint billing_checkout_sessions_provider_check check (provider <> ''),
  -- 32 crypto-random bytes, hex. Unguessable is the whole security property.
  ref text primary key
    constraint billing_checkout_sessions_ref_check check (ref <> '')
);

-- Supports the on-delete cascade; no query filters on org_id alone.
create index billing_checkout_sessions_org_idx on billing_checkout_sessions (org_id);
create index billing_checkout_sessions_create_idx on billing_checkout_sessions (create_time);

-- Every delivery as sent, so the provider's retries are safe and a failed payload
-- is replayable. Keyed by (provider, webhook_id), the provider's own namespace.
-- payload carries personal data: no RPC reads it, and reconcile prunes at 90 days.
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

-- Drives the prune. On the same expression it deletes by, so an unprocessed row
-- -- dated from received_at -- is covered rather than left to a seq scan.
create index billing_webhook_deliveries_prune_idx
  on billing_webhook_deliveries (coalesce(processed_at, received_at));

-- The reconcile pass reads only the rejected rows, and they are the rare ones.
-- Without this it seq-scans every delivery in the retention window, every pass.
create index billing_webhook_deliveries_rejected_idx
  on billing_webhook_deliveries (received_at)
  where error <> '';

-- +goose Down
drop table if exists billing_webhook_deliveries;
drop table if exists billing_checkout_sessions;
drop table if exists billing_subscriptions;

alter table billing_entitlement_history
  drop constraint if exists billing_entitlement_history_deletion_is_empty;
alter table billing_entitlement_history drop column if exists provider_product_id;
alter table billing_entitlement_history
  add constraint billing_entitlement_history_deletion_is_empty
    check (plan_slug is not null
      or (anchor_day is null and contract_ends_at is null
          and display_name_override is null and included_events_override is null
          and retention_days_override is null and trial_ends_at is null));

alter table billing_entitlements drop column if exists provider_product_id;
