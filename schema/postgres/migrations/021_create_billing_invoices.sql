-- +goose Up
-- Usage-based pricing: pug owns the price as well as the quota, and the ledger it
-- bills from. Nothing reads the new columns yet -- the code lands beside the
-- constraints it breaks, in the commits after this one. The price_cents drop is
-- the exception: the code it breaks lands here.

-- A deal's money. NULL on every org that is not a deal: a card's rates are Go.
alter table billing_entitlements
  add column flat_fee_cents bigint
    constraint billing_entitlements_flat_fee_check check (flat_fee_cents >= 0),
  add column rate_cents_per_million bigint
    constraint billing_entitlements_rate_check check (rate_cents_per_million >= 0),
  -- When these terms start pricing. An earlier period is priced on what it was
  -- sold under, so a renegotiation cannot reprice a month already used.
  add column terms_effective_at timestamptz;

-- The history is a snapshot of the row above, so it takes the money columns too.
-- The deletion constraint enumerates the value columns, so it is replaced.
alter table billing_entitlement_history
  add column flat_fee_cents bigint
    constraint billing_entitlement_history_flat_fee_check check (flat_fee_cents >= 0),
  add column rate_cents_per_million bigint
    constraint billing_entitlement_history_rate_check check (rate_cents_per_million >= 0);

alter table billing_entitlement_history
  drop constraint billing_entitlement_history_deletion_is_empty;

alter table billing_entitlement_history
  add constraint billing_entitlement_history_deletion_is_empty
    check (plan_slug is not null
      or (anchor_day is null and contract_ends_at is null
          and display_name_override is null and flat_fee_cents is null
          and included_events_override is null and provider_product_id is null
          and rate_cents_per_million is null and retention_days_override is null
          and trial_ends_at is null));

-- A mandate's recurring price is never what pug charges: usage is. Mirroring it
-- only invites someone to read it as the bill.
alter table billing_subscriptions drop column price_cents;

-- The ledger. One row per close, keyed (org, billed_from), written by the
-- invoicing pass and the payment webhooks; never pruned. No FK to orgs, like the
-- entitlement history -- the org being gone is when a bill gets asked about.
create table billing_invoices (
  amount_cents bigint not null,
  attempts int not null default 0,
  -- The clipped billable window: each close starts at the last billed_to, which
  -- is exclusive.
  billed_from date not null,
  billed_to date not null,
  carried_cents bigint not null default 0
    constraint billing_invoices_carried_check check (carried_cents >= 0),
  -- On a deferred or paid row: the invoice carrying its cents.
  covered_by char(20) references billing_invoices(id),
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
  -- The first charge, dated a notice window after the close, then each retry.
  next_attempt_at timestamptz,
  org_id char(20) not null,
  paid_at timestamptz,
  period_end timestamptz not null,
  period_start timestamptz not null,
  plan_slug varchar(50) not null
    constraint billing_invoices_plan_slug_check check (plan_slug <> ''),
  -- The rate card or deal terms this was priced on, snapshotted.
  pricing jsonb not null,
  -- Stamped with provider_sub_id at each charge; null before the first.
  provider text,
  provider_invoice_url text,
  provider_payment_id text,
  provider_sub_id text,
  status text not null
    constraint billing_invoices_status_check check (status <> ''),
  -- Added on top by the provider, from the settled payment; null until paid.
  tax_cents bigint
    constraint billing_invoices_tax_check check (tax_cents >= 0),
  update_time timestamptz not null default now(),
  usage_cents bigint not null
    constraint billing_invoices_usage_check check (usage_cents >= 0),
  -- The meter stamp the count came from.
  usage_computed_at timestamptz not null,

  constraint billing_invoices_window_check check (billed_from < billed_to),
  -- What is charged is this period plus what earlier periods deferred.
  constraint billing_invoices_amount_check
    check (amount_cents = usage_cents + carried_cents),
  -- A row is carried while it is parked or once its carrier settled it.
  constraint billing_invoices_covered_check
    check (covered_by is null or status in ('deferred', 'paid')),
  -- One close per start day; the clipping in the pass is what stops a day being
  -- billed twice.
  constraint billing_invoices_org_window_key unique (org_id, billed_from),
  constraint billing_invoices_payment_key unique (provider, provider_payment_id)
);

-- The pass reads by status; the rows it wants are the few not yet settled.
create index billing_invoices_status_idx on billing_invoices (status, next_attempt_at);

create trigger update_timestamp before
update on billing_invoices for each row execute procedure moddatetime(update_time);

-- Append-only, like the entitlement history: every transition, and who made it.
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

alter table billing_subscriptions
  add column price_cents bigint not null default 0
    constraint billing_subscriptions_price_check check (price_cents >= 0);
alter table billing_subscriptions alter column price_cents drop default;

alter table billing_entitlement_history
  drop constraint if exists billing_entitlement_history_deletion_is_empty,
  drop column if exists rate_cents_per_million,
  drop column if exists flat_fee_cents;

alter table billing_entitlement_history
  add constraint billing_entitlement_history_deletion_is_empty
    check (plan_slug is not null
      or (anchor_day is null and contract_ends_at is null
          and display_name_override is null and included_events_override is null
          and provider_product_id is null and retention_days_override is null
          and trial_ends_at is null));

alter table billing_entitlements
  drop column if exists terms_effective_at,
  drop column if exists rate_cents_per_million,
  drop column if exists flat_fee_cents;
