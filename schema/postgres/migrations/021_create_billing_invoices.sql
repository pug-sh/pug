-- +goose Up
-- Usage-based pricing: pug owns the price as well as the quota. A deal's money
-- lives on the entitlement row; the provider product is gone, since every org
-- authorizes against the one mandate product.

alter table billing_entitlements
  drop constraint billing_entitlements_custom_needs_quota,
  drop column provider_product_id,
  add column flat_fee_cents bigint
    constraint billing_entitlements_flat_fee_check check (flat_fee_cents >= 0),
  add column block_rate_cents bigint
    constraint billing_entitlements_block_rate_check check (block_rate_cents >= 0),
  add constraint billing_entitlements_custom_needs_price
    check (plan_slug <> 'custom' or flat_fee_cents is not null or block_rate_cents is not null);

alter table billing_entitlement_history
  drop constraint billing_entitlement_history_custom_needs_quota,
  drop constraint billing_entitlement_history_deletion_is_empty,
  drop column provider_product_id,
  add column flat_fee_cents bigint
    constraint billing_entitlement_history_flat_fee_check check (flat_fee_cents >= 0),
  add column block_rate_cents bigint
    constraint billing_entitlement_history_block_rate_check check (block_rate_cents >= 0),
  add constraint billing_entitlement_history_custom_needs_price
    check (plan_slug <> 'custom' or flat_fee_cents is not null or block_rate_cents is not null),
  add constraint billing_entitlement_history_deletion_is_empty
    check (plan_slug is not null
      or (anchor_day is null and contract_ends_at is null
          and display_name_override is null and included_events_override is null
          and retention_days_override is null and trial_ends_at is null
          and flat_fee_cents is null and block_rate_cents is null));

-- The mandate. on_demand is what the boundary admits; the two beside it are what
-- the invoicing pass clips the last period to. It keeps its default: false is a
-- real answer (not chargeable), and the safe one for a writer that forgets.
alter table billing_subscriptions
  add column on_demand boolean not null default false,
  add column cancel_at_period_end boolean not null default false,
  add column ended_at timestamptz;

-- The card in force when the checkout opened, carried onto the subscription the
-- delivery produces. The default only backfills a checkout opened before this
-- migration, then goes: the check forbids '', so every writer must name one.
alter table billing_checkout_sessions
  add column plan_slug varchar(50) not null default 'usage-2026-09'
    constraint billing_checkout_sessions_plan_slug_check check (plan_slug <> '');

alter table billing_checkout_sessions alter column plan_slug drop default;

-- The ledger. One row per (org, period), written by the invoicing pass and the
-- payment webhooks; never pruned.
create table billing_invoices (
  amount_cents bigint not null
    constraint billing_invoices_amount_check check (amount_cents >= 0),
  attempts int not null default 0,
  billed_from date not null,
  billed_to date not null,
  blocks bigint not null,
  create_time timestamptz not null default now(),
  currency varchar(3) not null
    constraint billing_invoices_currency_check check (currency ~ '^[A-Z]{3}$'),
  event_count bigint not null,
  failed_at timestamptz,
  id char(20) primary key,
  last_error_code text not null default '',
  -- Merchant-facing; never on the wire.
  last_error_message text not null default '',
  lines jsonb not null,
  next_attempt_at timestamptz,
  org_id char(20) not null references orgs(id) on delete cascade,
  paid_at timestamptz,
  period_end timestamptz not null,
  period_start timestamptz not null,
  plan_slug varchar(50) not null,
  pricing jsonb not null,
  provider text,
  provider_invoice_url text,
  provider_payment_id text,
  provider_sub_id text,
  status text not null
    constraint billing_invoices_status_check check (status <> ''),
  update_time timestamptz not null default now(),
  usage_computed_at timestamptz not null,

  constraint billing_invoices_org_period_key unique (org_id, period_start),
  constraint billing_invoices_payment_key unique (provider, provider_payment_id)
);

create index billing_invoices_org_idx on billing_invoices (org_id, period_start desc);
-- The pass reads by status; the rows it wants are the few not yet settled.
create index billing_invoices_status_idx on billing_invoices (status, next_attempt_at);

create trigger update_timestamp before
update on billing_invoices for each row execute procedure moddatetime(update_time);

create table billing_invoice_events (
  actor text not null
    constraint billing_invoice_events_actor_check check (actor <> ''),
  at timestamptz not null default now(),
  detail text not null default '',
  from_status text not null,
  id char(20) primary key,
  invoice_id char(20) not null references billing_invoices(id) on delete cascade,
  to_status text not null
);

create index billing_invoice_events_invoice_idx on billing_invoice_events (invoice_id, at);

-- +goose Down
drop table if exists billing_invoice_events;
drop table if exists billing_invoices;

alter table billing_checkout_sessions drop column if exists plan_slug;

alter table billing_subscriptions
  drop column if exists ended_at,
  drop column if exists cancel_at_period_end,
  drop column if exists on_demand;

alter table billing_entitlement_history
  drop constraint if exists billing_entitlement_history_deletion_is_empty,
  drop constraint if exists billing_entitlement_history_custom_needs_price,
  drop column if exists block_rate_cents,
  drop column if exists flat_fee_cents,
  add column provider_product_id text
    constraint billing_entitlement_history_provider_product_check
      check (provider_product_id is null or provider_product_id <> ''),
  add constraint billing_entitlement_history_custom_needs_quota
    check (plan_slug <> 'custom' or included_events_override is not null),
  add constraint billing_entitlement_history_deletion_is_empty
    check (plan_slug is not null
      or (anchor_day is null and contract_ends_at is null
          and display_name_override is null and included_events_override is null
          and provider_product_id is null and retention_days_override is null
          and trial_ends_at is null));

alter table billing_entitlements
  drop constraint if exists billing_entitlements_custom_needs_price,
  drop column if exists block_rate_cents,
  drop column if exists flat_fee_cents,
  add column provider_product_id text
    constraint billing_entitlements_provider_product_check
      check (provider_product_id is null or provider_product_id <> ''),
  add constraint billing_entitlements_custom_needs_quota
    check (plan_slug <> 'custom' or included_events_override is not null);
