-- +goose Up
-- A deal's money. NULL on every org that is not a deal: a card's rates are Go.
alter table billing_entitlements
  add column flat_fee_cents bigint
    constraint billing_entitlements_flat_fee_check check (flat_fee_cents >= 0),
  add column rate_cents_per_million bigint
    constraint billing_entitlements_rate_check check (rate_cents_per_million >= 0);

-- An allowance alone is a free tier nobody agreed to. `> 0` rather than not-null,
-- so a hand-written zero cannot turn a deal into one.
alter table billing_entitlements
  drop constraint billing_entitlements_custom_needs_quota,
  add constraint billing_entitlements_custom_needs_price
    check (plan_slug <> 'custom'
      or coalesce(flat_fee_cents, 0) > 0 or coalesce(rate_cents_per_million, 0) > 0);

-- The history is a snapshot of the row above, so it takes the money columns too.
alter table billing_entitlement_history
  add column flat_fee_cents bigint
    constraint billing_entitlement_history_flat_fee_check check (flat_fee_cents >= 0),
  add column rate_cents_per_million bigint
    constraint billing_entitlement_history_rate_check check (rate_cents_per_million >= 0);

alter table billing_entitlement_history
  drop constraint billing_entitlement_history_custom_needs_quota,
  add constraint billing_entitlement_history_custom_needs_price
    check (plan_slug <> 'custom'
      or coalesce(flat_fee_cents, 0) > 0 or coalesce(rate_cents_per_million, 0) > 0);

-- The deletion marker enumerates every value column, so it has to learn the new
-- ones: without this a snapshot could record a deletion and still carry a price.
alter table billing_entitlement_history
  drop constraint billing_entitlement_history_deletion_is_empty,
  add constraint billing_entitlement_history_deletion_is_empty
    check (plan_slug is not null
      or (anchor_day is null and contract_ends_at is null
          and display_name_override is null and flat_fee_cents is null
          and included_events_override is null and rate_cents_per_million is null
          and retention_days_override is null and trial_ends_at is null));

-- +goose Down
-- A custom row priced only by the columns below would fail the restored quota
-- constraint, and normalizing it is also what lets Up re-apply cleanly.
update billing_entitlement_history set plan_slug = 'free'
  where plan_slug = 'custom' and included_events_override is null;
update billing_entitlements set plan_slug = 'free'
  where plan_slug = 'custom' and included_events_override is null;

alter table billing_entitlement_history
  drop constraint billing_entitlement_history_deletion_is_empty,
  drop constraint billing_entitlement_history_custom_needs_price,
  drop column flat_fee_cents,
  drop column rate_cents_per_million;

alter table billing_entitlement_history
  add constraint billing_entitlement_history_custom_needs_quota
    check (plan_slug <> 'custom' or included_events_override is not null),
  add constraint billing_entitlement_history_deletion_is_empty
    check (plan_slug is not null
      or (anchor_day is null and contract_ends_at is null
          and display_name_override is null and included_events_override is null
          and retention_days_override is null and trial_ends_at is null));

alter table billing_entitlements
  drop constraint billing_entitlements_custom_needs_price,
  drop column flat_fee_cents,
  drop column rate_cents_per_million;

alter table billing_entitlements
  add constraint billing_entitlements_custom_needs_quota
    check (plan_slug <> 'custom' or included_events_override is not null);
