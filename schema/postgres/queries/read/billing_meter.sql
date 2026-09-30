-- name: ListLiveBillingSubscriptionsForMeter :many
-- The meter's work list: every live subscription of one provider whose period the
-- provider has reported. One per org, by the partial unique index.
select * from billing_subscriptions
where provider = @provider
  and status in ('active', 'past_due')
  and current_period_start is not null
  and current_period_end is not null
order by org_id;

-- name: SumOrgUsageDaily :one
-- An org's metered events over whole UTC days [from_day, to_day). Billing reads
-- the meter's Postgres copy, never ClickHouse.
select coalesce(sum(event_count), 0)::bigint as event_count
from usage_daily
where org_id = @org_id and day >= @from_day and day < @to_day;

-- name: GetBillingMeterPeriod :one
select * from billing_meter_periods
where org_id = @org_id and period_start = @period_start;

-- name: GetPreviousBillingMeterPeriod :one
-- The org's latest stated period before this one: what a contiguous window
-- carries from.
select * from billing_meter_periods
where org_id = @org_id and period_start < @period_start
order by period_start desc
limit 1;

-- name: ListLiveCustomDealProducts :many
-- Reconcile's check that every live deal's product bills every tier of the plan
-- the deal is pinned to.
select s.org_id, e.provider_product_id::text as provider_product_id,
  e.base_plan_slug::text as base_plan_slug
from billing_subscriptions s
join billing_entitlements e on e.org_id = s.org_id
where s.provider = @provider
  and s.status in ('active', 'past_due')
  and s.plan_slug = 'custom'
  and e.provider_product_id is not null
order by s.org_id;
