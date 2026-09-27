-- +goose Up
-- Usage billing: the trial goes, the fixed-price tiers go, and a deal is defined by
-- the provider product it is bought against rather than by a quota.

-- A row naming a retired fixed-price tier becomes free: without a subscription
-- nothing billed it, and its overrides keep resolving on free.
update billing_entitlements set plan_slug = 'free'
where plan_slug in ('trial', 'starter', 'growth', 'scale');

alter table billing_entitlements drop constraint billing_entitlements_custom_needs_quota;
-- A deal's price is its product, so a deal cannot exist without one, and a product
-- names a deal and nothing else. Validated: an existing custom row with no product
-- fails the migration rather than being rewritten.
alter table billing_entitlements add constraint billing_entitlements_custom_needs_product
  check ((plan_slug = 'custom') = (provider_product_id is not null));
alter table billing_entitlements drop column trial_ends_at;

-- History keeps past snapshots as they were written, so the new rule binds new rows
-- only.
alter table billing_entitlement_history drop constraint billing_entitlement_history_custom_needs_quota;
alter table billing_entitlement_history add constraint billing_entitlement_history_custom_needs_product
  check ((plan_slug = 'custom') = (provider_product_id is not null)) not valid;
alter table billing_entitlement_history drop constraint billing_entitlement_history_deletion_is_empty;
alter table billing_entitlement_history drop column trial_ends_at;
alter table billing_entitlement_history add constraint billing_entitlement_history_deletion_is_empty
  check (plan_slug is not null
    or (anchor_day is null and contract_ends_at is null
        and display_name_override is null and included_events_override is null
        and provider_product_id is null and retention_days_override is null));

-- +goose Down
alter table billing_entitlement_history drop constraint billing_entitlement_history_deletion_is_empty;
alter table billing_entitlement_history add column trial_ends_at timestamptz;
alter table billing_entitlement_history add constraint billing_entitlement_history_deletion_is_empty
  check (plan_slug is not null
    or (anchor_day is null and contract_ends_at is null
        and display_name_override is null and included_events_override is null
        and provider_product_id is null and retention_days_override is null
        and trial_ends_at is null));
alter table billing_entitlement_history drop constraint billing_entitlement_history_custom_needs_product;
alter table billing_entitlement_history add constraint billing_entitlement_history_custom_needs_quota
  check (plan_slug <> 'custom' or included_events_override is not null) not valid;

alter table billing_entitlements add column trial_ends_at timestamptz;
alter table billing_entitlements drop constraint billing_entitlements_custom_needs_product;
-- Fails if a deal carries no events override: set one first.
alter table billing_entitlements add constraint billing_entitlements_custom_needs_quota
  check (plan_slug <> 'custom' or included_events_override is not null);
