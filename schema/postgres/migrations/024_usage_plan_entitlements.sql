-- +goose Up
-- Usage billing: the trial goes, the fixed-price tiers go, and a deal is defined by
-- the provider product it is bought against and the plan it splits over, rather
-- than by a quota.
--
-- trial, starter, growth and scale leave the catalog outright rather than retiring:
-- no subscription on any of them was ever live, so nothing can still hold one. It is
-- the one deletion billing.md §4.2 allows.

-- The new checks below refuse a deal with no product, a product on anything but a
-- deal, and a slug that is neither a state nor a removed tier. Postgres would name
-- only the constraint, so the rows are named here first. Fix each with the previous
-- release's `pug billing set` or `clear` and re-run: give a deal its
-- --provider-product, or clear a stray one with --provider-product "". This
-- release's own writes need the column 024 adds, so they fail until it lands.
-- +goose StatementBegin
do $$
declare
  unplaceable text;
begin
  select string_agg(format('%s (%s, %s)', org_id, plan_slug,
           coalesce('product ' || provider_product_id, 'no product')), '; ' order by org_id)
    into unplaceable
    from billing_entitlements
   where (plan_slug = 'custom') <> (provider_product_id is not null)
      or plan_slug not in ('free', 'custom', 'trial', 'starter', 'growth', 'scale');
  if unplaceable is not null then
    raise exception 'migration 024 cannot place these rows (a deal needs a provider product, '
      'a product needs a deal, and a plan must be free, custom or a removed tier): %', unplaceable;
  end if;
end $$;
-- +goose StatementEnd

-- The plan a deal splits over, pinned when its product is set: the product carries
-- one meter per tier of that plan, so a reprice must not move it.
alter table billing_entitlements
  add column base_plan_slug varchar(50)
    constraint billing_entitlements_base_plan_check check (base_plan_slug <> '');
alter table billing_entitlement_history
  add column base_plan_slug varchar(50)
    constraint billing_entitlement_history_base_plan_check check (base_plan_slug <> '');

-- History keeps past snapshots as they were written -- a deal recorded before usage
-- billing, with a quota and no product, and a trial's end -- so the new rules bind
-- new rows only.
alter table billing_entitlement_history
  drop constraint billing_entitlement_history_custom_needs_quota;
alter table billing_entitlement_history
  add constraint billing_entitlement_history_custom_needs_product
    check ((plan_slug = 'custom') = (provider_product_id is not null)) not valid;
alter table billing_entitlement_history
  add constraint billing_entitlement_history_custom_needs_base_plan
    check ((plan_slug = 'custom') = (base_plan_slug is not null)) not valid;
alter table billing_entitlement_history
  drop constraint billing_entitlement_history_deletion_is_empty;
alter table billing_entitlement_history
  add constraint billing_entitlement_history_deletion_is_empty
    check (plan_slug is not null
      or (anchor_day is null and base_plan_slug is null and contract_ends_at is null
          and display_name_override is null and included_events_override is null
          and provider_product_id is null and retention_days_override is null
          and trial_ends_at is null));

-- A row naming a removed tier becomes free: without a subscription nothing billed
-- it, and its overrides keep resolving on free. An existing deal is pinned to the one
-- plan there is. Both are recorded, as every other write to the row is.
with moved as (
  update billing_entitlements set plan_slug = 'free'
  where plan_slug in ('trial', 'starter', 'growth', 'scale')
  returning *
), pinned as (
  update billing_entitlements set base_plan_slug = 'usage-2026-10'
  where plan_slug = 'custom'
  returning *
)
insert into billing_entitlement_history (
  actor, anchor_day, base_plan_slug, contract_ends_at, display_name_override, id,
  included_events_override, note, org_id, plan_slug, provider_product_id,
  retention_days_override
)
select 'migration/024', anchor_day, base_plan_slug, contract_ends_at, display_name_override,
  left(replace(gen_random_uuid()::text, '-', ''), 20),
  included_events_override, note, org_id, plan_slug, provider_product_id,
  retention_days_override
from (select * from moved union all select * from pinned) changed;

alter table billing_entitlements drop constraint billing_entitlements_custom_needs_quota;
-- A deal's price is its product, so a deal cannot exist without one, and a product
-- names a deal and nothing else.
alter table billing_entitlements
  add constraint billing_entitlements_custom_needs_product
    check ((plan_slug = 'custom') = (provider_product_id is not null));
alter table billing_entitlements
  add constraint billing_entitlements_custom_needs_base_plan
    check ((plan_slug = 'custom') = (base_plan_slug is not null));
-- The row holds a state, never a plan: a usage plan is held only through a
-- subscription. States do not change on a reprice, so this is no second catalog.
alter table billing_entitlements
  add constraint billing_entitlements_plan_slug_state_check
    check (plan_slug in ('free', 'custom'));
alter table billing_entitlements drop column trial_ends_at;

-- +goose Down
-- The rewrites stay: nothing but the history records which tier a free row came
-- from, and the snapshots 024 appended stay with it.
alter table billing_entitlements add column trial_ends_at timestamptz;
alter table billing_entitlements drop constraint billing_entitlements_plan_slug_state_check;
alter table billing_entitlements drop constraint billing_entitlements_custom_needs_base_plan;
alter table billing_entitlements drop constraint billing_entitlements_custom_needs_product;
alter table billing_entitlements drop column base_plan_slug;
-- Fails if a deal carries no events override: set one first.
alter table billing_entitlements
  add constraint billing_entitlements_custom_needs_quota
    check (plan_slug <> 'custom' or included_events_override is not null);

alter table billing_entitlement_history
  drop constraint billing_entitlement_history_deletion_is_empty;
alter table billing_entitlement_history
  drop constraint billing_entitlement_history_custom_needs_base_plan;
alter table billing_entitlement_history
  drop constraint billing_entitlement_history_custom_needs_product;
alter table billing_entitlement_history drop column base_plan_slug;
alter table billing_entitlement_history
  add constraint billing_entitlement_history_custom_needs_quota
    check (plan_slug <> 'custom' or included_events_override is not null) not valid;
alter table billing_entitlement_history
  add constraint billing_entitlement_history_deletion_is_empty
    check (plan_slug is not null
      or (anchor_day is null and contract_ends_at is null
          and display_name_override is null and included_events_override is null
          and provider_product_id is null and retention_days_override is null
          and trial_ends_at is null));
