-- name: GetOrgEntitlement :one
-- Left join rather than two reads: most orgs have no entitlement row, and that
-- is the ordinary answer, not an error.
select
  o.create_time as org_create_time,
  e.anchor_day,
  e.contract_ends_at,
  e.display_name_override,
  e.included_events_override,
  e.note,
  e.plan_slug,
  e.provider_product_id,
  e.retention_days_override,
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
-- Every paid org should have a provider subscription. A row here is an org
-- entitled to something nobody is charged for.
select e.org_id, e.plan_slug
from billing_entitlements e
left join billing_subscriptions s
  on s.org_id = e.org_id and s.status in ('active', 'past_due')
where e.plan_slug not in ('free', 'trial')
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

-- name: ListBillingSubscriptionsByOrg :many
-- The operator's view: every stored row, newest first. Neither provider-scoped nor
-- live-only -- a lapsed row is most of what `show` exists to explain.
select * from billing_subscriptions
where org_id = @org_id
order by create_time desc;
