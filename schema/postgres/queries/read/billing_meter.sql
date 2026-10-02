-- name: ListLiveBillingSubscriptionsForMeter :many
-- The meter's work list: every live subscription of one provider. One per org, by
-- the partial unique index. A row the provider has reported no period for is
-- listed too: the pass cannot meter it, and fails it rather than leaving it out.
select * from billing_subscriptions
where provider = @provider
  and status in ('active', 'past_due')
order by org_id;

-- name: CountLiveBillingSubscriptions :one
-- Every live subscription, whichever provider holds it: what the meter binary
-- checks before deciding that a deployment with nothing to meter is healthy.
select count(*) from billing_subscriptions
where status in ('active', 'past_due');

-- name: GetUsageComputedAt :one
-- When the usage pass last verified a count, across every org: it refreshes every
-- org's period in one pass, and refreshes none from a read it cannot trust. NULL
-- means it has never run. The meter sums the day cells that pass writes, so it
-- refuses to state from them once this is stale.
select max(usage_computed_at)::timestamptz as usage_computed_at from usage_periods;

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
-- The window a new one follows: the org's other period whose days end latest. A
-- period of the same subscription ends with its window, since a renewal is
-- continuous; another subscription's ends at summed_through, the last day it was
-- seen live, so the days after it are not re-billed through a carry. Not the
-- latest period_start: a cutover that straddles the old subscription's renewal
-- starts before that renewal's period, which it must follow. On a tie the later
-- period, which has carried the other already.
select * from billing_meter_periods
where org_id = @org_id and period_start <> @period_start
order by case when provider_sub_id = @provider_sub_id then window_end else summed_through end desc,
  period_start desc
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
