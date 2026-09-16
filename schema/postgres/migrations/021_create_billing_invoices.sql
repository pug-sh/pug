-- +goose Up
-- Usage-based pricing: pug owns the price as well as the quota, and the ledger it
-- bills from. The ledger's own tables are still unread -- the pass lands later --
-- but everything the resolver touches lands beside the code it breaks.

-- A deal's money. NULL on every org that is not a deal: a card's rates are Go.
alter table billing_entitlements
  add column flat_fee_cents bigint
    constraint billing_entitlements_flat_fee_check check (flat_fee_cents >= 0),
  add column rate_cents_per_million bigint
    constraint billing_entitlements_rate_check check (rate_cents_per_million >= 0),
  -- When these terms start pricing. An earlier period is priced on what it was
  -- sold under, so a renegotiation cannot reprice a month already used.
  add column terms_effective_at timestamptz;

-- free and trial stop being plans, so a row carries a trial end, an anchor day or
-- an override with no pin at all. NULL is "no pin"; create_time is what says the
-- row exists.
alter table billing_entitlements alter column plan_slug drop not null;

-- An allowance alone is a free tier nobody agreed to. `> 0` rather than not-null,
-- so a hand-written zero cannot turn a deal into one.
alter table billing_entitlements
  drop constraint billing_entitlements_custom_needs_quota,
  add constraint billing_entitlements_custom_needs_price
    check (plan_slug <> 'custom'
      or coalesce(flat_fee_cents, 0) > 0 or coalesce(rate_cents_per_million, 0) > 0);

-- The history is a snapshot of the row above, so it takes the money columns too.
-- The deletion constraint enumerates the value columns, so it is replaced.
alter table billing_entitlement_history
  add column flat_fee_cents bigint
    constraint billing_entitlement_history_flat_fee_check check (flat_fee_cents >= 0),
  add column rate_cents_per_million bigint
    constraint billing_entitlement_history_rate_check check (rate_cents_per_million >= 0),
  add column terms_effective_at timestamptz,
  -- A deletion used to be encoded as a NULL plan_slug. Now that a live row can
  -- hold one, the marker has to be its own column.
  add column deleted boolean not null default false;

alter table billing_entitlement_history
  drop constraint billing_entitlement_history_custom_needs_quota,
  drop constraint billing_entitlement_history_deletion_is_empty,
  add constraint billing_entitlement_history_custom_needs_price
    check (plan_slug <> 'custom'
      or coalesce(flat_fee_cents, 0) > 0 or coalesce(rate_cents_per_million, 0) > 0),
  -- A snapshot of a deletion carries no values; actor and note still describe it.
  add constraint billing_entitlement_history_deletion_is_empty
    check (not deleted
      or (anchor_day is null and contract_ends_at is null
          and display_name_override is null and flat_fee_cents is null
          and included_events_override is null and plan_slug is null
          and provider_product_id is null and rate_cents_per_million is null
          and retention_days_override is null and terms_effective_at is null
          and trial_ends_at is null));

-- A mandate's recurring price is never what pug charges: usage is. Mirroring it
-- only invites someone to read it as the bill.
alter table billing_subscriptions drop column price_cents;

-- on_demand is what the webhook admits; the two beside it are what a close clips
-- the last period to. They keep their default, because false is a real answer --
-- not chargeable, not ending -- and the safe one for a writer that forgets.
alter table billing_subscriptions
  add column on_demand boolean not null default false,
  add column cancel_at_period_end boolean not null default false,
  add column ended_at timestamptz;

-- The card in force when the checkout opened, carried onto the subscription the
-- delivery produces, so a retirement mid-checkout cannot move the price the buyer
-- agreed to. No default: nothing has opened a checkout, and every writer names one.
alter table billing_checkout_sessions
  add column plan_slug varchar(50) not null
    constraint billing_checkout_sessions_plan_slug_check check (plan_slug <> '');

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
    constraint billing_invoice_events_to_status_check check (to_status <> '')
);

create index billing_invoice_events_invoice_idx on billing_invoice_events (invoice_id, at);

-- +goose Down
drop table if exists billing_invoice_events;
drop table if exists billing_invoices;

-- The pinned card is what an open checkout's row is for, and the column is going.
-- These are pruned ephemera either way, and a row left behind with no slug is what
-- a re-applied Up would refuse to add its not-null column to.
delete from billing_checkout_sessions;

alter table billing_checkout_sessions drop column if exists plan_slug;

alter table billing_subscriptions
  drop column if exists ended_at,
  drop column if exists cancel_at_period_end,
  drop column if exists on_demand;

alter table billing_subscriptions
  add column price_cents bigint not null default 0
    constraint billing_subscriptions_price_check check (price_cents >= 0);
alter table billing_subscriptions alter column price_cents drop default;

-- 021 allows rows the constraints below cannot express: a pin-less row, and a
-- deal whose price is in the columns this rollback drops. Normalizing every one
-- is also what keeps a re-applied Up from rejecting what Down left.
update billing_entitlement_history set plan_slug = 'free'
  where not deleted and (plan_slug is null or plan_slug = 'custom');

alter table billing_entitlement_history
  drop constraint if exists billing_entitlement_history_deletion_is_empty,
  drop constraint if exists billing_entitlement_history_custom_needs_price,
  drop column if exists deleted,
  drop column if exists terms_effective_at,
  drop column if exists rate_cents_per_million,
  drop column if exists flat_fee_cents;

alter table billing_entitlement_history
  add constraint billing_entitlement_history_custom_needs_quota
    check (plan_slug <> 'custom' or included_events_override is not null),
  add constraint billing_entitlement_history_deletion_is_empty
    check (plan_slug is not null
      or (anchor_day is null and contract_ends_at is null
          and display_name_override is null and included_events_override is null
          and provider_product_id is null and retention_days_override is null
          and trial_ends_at is null));

-- As above: no deal survives a rollback that drops the money it was priced on.
update billing_entitlements set plan_slug = 'free'
  where plan_slug is null or plan_slug = 'custom';

alter table billing_entitlements
  drop constraint if exists billing_entitlements_custom_needs_price,
  drop column if exists terms_effective_at,
  drop column if exists rate_cents_per_million,
  drop column if exists flat_fee_cents;

alter table billing_entitlements alter column plan_slug set not null;

alter table billing_entitlements
  add constraint billing_entitlements_custom_needs_quota
    check (plan_slug <> 'custom' or included_events_override is not null);
