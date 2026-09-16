-- name: GetOrgEntitlement :one
-- Left join rather than two reads: most orgs have no entitlement row, and that
-- is the ordinary answer, not an error.
select
  o.create_time as org_create_time,
  -- What says the row exists: plan_slug is nullable now, so an org can hold a
  -- trial end or an anchor day with no pin at all.
  e.create_time as entitlement_create_time,
  e.anchor_day,
  e.contract_ends_at,
  e.display_name_override,
  e.flat_fee_cents,
  e.included_events_override,
  e.note,
  e.plan_slug,
  e.rate_cents_per_million,
  e.retention_days_override,
  e.terms_effective_at,
  e.trial_ends_at
from orgs o
left join billing_entitlements e on e.org_id = o.id
where o.id = @org_id;

-- name: ListBillingEntitlementHistory :many
select * from billing_entitlement_history
where org_id = @org_id
order by changed_at desc, id desc
limit @row_limit;

-- name: GetLiveBillingSubscription :one
-- The one row that supplies a plan. The partial unique index is what makes "the"
-- correct: an org may hold a winding-down row beside it, and that one is not live.
select * from billing_subscriptions
where org_id = @org_id and status in ('active', 'past_due');

-- name: ListBillingSubscriptionsByProvider :many
-- Every row the reconcile pass re-reads, live or not: a cancellation that never
-- arrived is exactly what it exists to find.
select * from billing_subscriptions
where provider = @provider
order by id
limit @row_limit offset @row_offset;

-- name: ListPaidEntitlementsWithoutLiveSubscription :many
-- A deal in force with no mandate behind it: an org entitled to something nobody
-- is charged for. A pinned card is grandfathering rather than a grant, so it is
-- not one of these -- an org with no mandate simply is not invoiced.
select e.org_id, e.plan_slug
from billing_entitlements e
left join billing_subscriptions s
  on s.org_id = e.org_id and s.status in ('active', 'past_due')
where e.plan_slug = 'custom'
  and (e.contract_ends_at is null or e.contract_ends_at > now())
  and s.org_id is null
order by e.org_id;

-- name: GetLatestBillingSubscription :one
-- Any row, newest first -- not only a live one. A customer whose subscription
-- lapsed still has invoices to fetch and a card to re-add. Provider-scoped, or a
-- cutover hands provider A's customer id to provider B.
select * from billing_subscriptions
where org_id = @org_id and provider = @provider
order by create_time desc
limit 1;

-- name: ListRecentRejectedBillingWebhookDeliveries :many
-- Deliveries pug accepted and did not apply. Nothing else surfaces them: one that
-- was not applied wrote no subscription row for the walk above to find.
select provider, webhook_id, event_type, error
from billing_webhook_deliveries
where error <> '' and received_at >= @since
order by received_at;

-- name: ListStrandedBillingWebhookDeliveries :many
-- Deliveries that never settled: every retry failed, so the row carries no error
-- either and the query above cannot see it. The provider's retries are spent
-- long before stale_before, so a row still unprocessed here will never process.
select provider, webhook_id, event_type
from billing_webhook_deliveries
where processed_at is null and received_at < @stale_before
order by received_at;

-- name: ListBillingSubscriptionsByOrg :many
-- The operator's view: every stored row, newest first. Neither provider-scoped nor
-- live-only -- a lapsed row is most of what `show` exists to explain.
select * from billing_subscriptions
where org_id = @org_id
order by create_time desc;

-- name: ListBillingInvoiceOrgs :many
-- Every org a close considers: one that ever held a mandate, or one on a deal,
-- lapsed or not, since a lapsed deal still owes the periods it ran through. A free
-- org gets no invoice.
select o.id, o.create_time
from orgs o
left join billing_entitlements e on e.org_id = o.id
where e.plan_slug = 'custom'
   or exists (select 1 from billing_subscriptions s where s.org_id = o.id)
order by o.id;

-- name: GetBillingInvoiceBilledTo :one
-- Where the org's billing has reached: each close starts here, so no day is
-- billed twice however the period moves. Any status, void included -- a voided
-- day was billed and stopped, not left unbilled.
select max(billed_to)::date from billing_invoices
where org_id = @org_id;
