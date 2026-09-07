-- name: LockBillingEntitlementOrg :exec
-- Take this first, on the writing tx: the for-update read locks nothing when the
-- org has no row yet, and off a tx this lock dies with its own statement.
select pg_advisory_xact_lock(hashtext('billing_entitlement:' || @org_id::text));

-- name: GetBillingEntitlementForUpdate :one
-- Returns no rows for an org that has never been touched, which is normal.
select * from billing_entitlements where org_id = @org_id for update;

-- name: UpsertBillingEntitlement :one
-- Full replace, never coalesce: the caller has already merged its change over
-- the locked row.
insert into billing_entitlements (
  anchor_day, contract_ends_at, display_name_override,
  included_events_override, note, org_id, plan_slug, provider_product_id, trial_ends_at
) values (
  @anchor_day, @contract_ends_at, @display_name_override,
  @included_events_override, @note, @org_id, @plan_slug, @provider_product_id, @trial_ends_at
)
on conflict (org_id) do update
set anchor_day = excluded.anchor_day,
    contract_ends_at = excluded.contract_ends_at,
    display_name_override = excluded.display_name_override,
    included_events_override = excluded.included_events_override,
    note = excluded.note,
    plan_slug = excluded.plan_slug,
    provider_product_id = excluded.provider_product_id,
    trial_ends_at = excluded.trial_ends_at
returning *;

-- name: DeleteBillingEntitlement :execrows
delete from billing_entitlements where org_id = @org_id;

-- name: InsertBillingEntitlementHistory :exec
insert into billing_entitlement_history (
  actor, anchor_day, contract_ends_at, display_name_override,
  id, included_events_override, note, org_id, plan_slug, provider_product_id, trial_ends_at
) values (
  @actor, @anchor_day, @contract_ends_at, @display_name_override,
  @id, @included_events_override, @note, @org_id, @plan_slug, @provider_product_id, @trial_ends_at
);

-- name: InsertBillingWebhookDelivery :one
-- The provider's retry reuses its webhook id, so the primary key dedups it. The
-- returned row is what tells a retry apart from a first delivery: an unprocessed
-- row means the last attempt died mid-apply and must be re-applied, so this
-- deliberately does not swallow the conflict.
insert into billing_webhook_deliveries (event_type, payload, provider, webhook_id)
values (@event_type, @payload, @provider, @webhook_id)
on conflict (provider, webhook_id) do update
  set event_type = billing_webhook_deliveries.event_type
returning *;

-- name: MarkBillingWebhookDeliveryProcessed :execrows
-- The row is guaranteed by the insert that opened the delivery, so zero rows is a
-- fault rather than a benign no-op.
update billing_webhook_deliveries
set processed_at = now(), error = @error
where provider = @provider and webhook_id = @webhook_id;

-- name: PruneBillingWebhookDeliveries :execrows
-- The payload holds personal data replay needs and nothing else does, so it is
-- kept for a window rather than forever. Unprocessed rows are never pruned: they
-- are the ones still worth replaying.
delete from billing_webhook_deliveries
where processed_at is not null and processed_at < @older_than;

-- name: ApplyBillingSubscription :execrows
-- The mirror write: one statement, three callers. CAS on provider_updated_at, which
-- is when a payload ARRIVED, so a delivery that overtakes another is what this
-- orders -- reconcile corrects the rest. org_id is never updated: attribution is
-- decided once, on first sight.
insert into billing_subscriptions (
  currency, current_period_end, current_period_start, id, org_id, plan_slug,
  price_cents, provider, provider_customer_id, provider_status, provider_sub_id,
  provider_updated_at, status
) values (
  @currency, @current_period_end, @current_period_start, @id, @org_id, @plan_slug,
  @price_cents, @provider, @provider_customer_id, @provider_status, @provider_sub_id,
  @provider_updated_at, @status
)
on conflict (provider, provider_sub_id) do update
set currency = excluded.currency,
    current_period_end = excluded.current_period_end,
    current_period_start = excluded.current_period_start,
    plan_slug = excluded.plan_slug,
    price_cents = excluded.price_cents,
    provider_customer_id = excluded.provider_customer_id,
    provider_status = excluded.provider_status,
    provider_updated_at = excluded.provider_updated_at,
    status = excluded.status
where billing_subscriptions.provider_updated_at <= excluded.provider_updated_at;

-- name: ListBillingSubscriptionOrgsByProviderCustomerID :many
-- Attribution fallback when a delivery carries no org_id metadata. Two rows is
-- the answer that matters: one buyer purchasing for two orgs shares a provider
-- customer, and nothing here can tell those apart, so the caller rejects the
-- delivery rather than attributing it to a guess. metadata.org_id is tried first
-- for that reason.
select distinct org_id from billing_subscriptions
where provider = @provider and provider_customer_id = @provider_customer_id
limit 2;
