-- name: GetOrgEntitlement :one
-- Left join rather than two reads: most orgs have no entitlement row, and that
-- is the ordinary answer, not an error.
select
  o.create_time as org_create_time,
  e.create_time as entitlement_create_time,
  e.anchor_day,
  e.contract_ends_at,
  e.display_name_override,
  e.included_events_override,
  e.note,
  e.plan_slug,
  e.retention_days_override,
  e.trial_ends_at,
  e.flat_fee_cents,
  e.block_rate_cents,
  e.terms_effective_at
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
-- A deal in force with no mandate behind it is an org entitled to something
-- nobody is charged for. A pinned card is grandfathering, not a grant.
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

-- name: GetBillingInvoice :one
select * from billing_invoices where id = @id;

-- name: GetBillingInvoiceByPayment :one
select * from billing_invoices
where provider = @provider and provider_payment_id = @provider_payment_id;

-- name: ListBillingInvoicesByOrg :many
select * from billing_invoices
where org_id = @org_id
order by period_start desc
limit @row_limit;

-- name: ListDueBillingInvoices :many
-- Open and failed rows whose retry date has come; the pass charges these.
select * from billing_invoices
where status in ('open', 'failed') and next_attempt_at <= @now
order by next_attempt_at
limit @row_limit;

-- name: ListBillingInvoicesByStatusBefore :many
-- The settle reads: charging rows older than a few minutes, charged rows older
-- than an hour.
select * from billing_invoices
where status = @status and update_time < @before
order by update_time
limit @row_limit;

-- name: HasDunningBillingInvoice :one
-- The one ledger read on the dashboard path: PAST_DUE is derived from it.
select exists (
  select 1 from billing_invoices
  where org_id = @org_id and status in ('failed', 'uncollectible')
);

-- name: ListBillingInvoiceOrgs :many
-- Every org the close step considers: one that ever held a mandate, or one on a
-- deal. A free org gets no invoice.
select o.id, o.create_time, e.anchor_day
from orgs o
left join billing_entitlements e on e.org_id = o.id
where e.plan_slug = 'custom'
   or exists (select 1 from billing_subscriptions s where s.org_id = o.id)
order by o.id;

-- name: ListUnbilledUsagePeriods :many
-- Closed periods over the free allowance for orgs with no live mandate: what the
-- free tier costs, reported and never billed.
select p.org_id, p.period_start, p.period_end, p.event_count
from usage_periods p
where p.event_count > @min_events
  and p.period_end <= @closed_before and p.period_end > @since
  and not exists (
    select 1 from billing_subscriptions s
    where s.org_id = p.org_id and s.status in ('active', 'past_due')
  )
order by p.org_id, p.period_start;

-- name: ListLiveBillingSubscriptionsByProvider :many
-- The mandates whose next_billing_date the pass pins.
select * from billing_subscriptions
where provider = @provider and status in ('active', 'past_due')
order by id
limit @row_limit offset @row_offset;

-- name: ExistsBillingInvoiceForPeriod :one
select exists (
  select 1 from billing_invoices where org_id = @org_id and period_start = @period_start
);
