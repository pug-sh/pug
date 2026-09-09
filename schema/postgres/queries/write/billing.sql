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
  included_events_override, note, org_id, plan_slug, provider_product_id,
  retention_days_override, trial_ends_at
) values (
  @anchor_day, @contract_ends_at, @display_name_override,
  @included_events_override, @note, @org_id, @plan_slug, @provider_product_id,
  @retention_days_override, @trial_ends_at
)
on conflict (org_id) do update
set anchor_day = excluded.anchor_day,
    contract_ends_at = excluded.contract_ends_at,
    display_name_override = excluded.display_name_override,
    included_events_override = excluded.included_events_override,
    note = excluded.note,
    plan_slug = excluded.plan_slug,
    provider_product_id = excluded.provider_product_id,
    retention_days_override = excluded.retention_days_override,
    trial_ends_at = excluded.trial_ends_at
returning *;

-- name: DeleteBillingEntitlement :execrows
delete from billing_entitlements where org_id = @org_id;

-- name: InsertBillingEntitlementHistory :exec
insert into billing_entitlement_history (
  actor, anchor_day, contract_ends_at, display_name_override,
  id, included_events_override, note, org_id, plan_slug, provider_product_id,
  retention_days_override, trial_ends_at
) values (
  @actor, @anchor_day, @contract_ends_at, @display_name_override,
  @id, @included_events_override, @note, @org_id, @plan_slug, @provider_product_id,
  @retention_days_override, @trial_ends_at
);

-- name: InsertBillingWebhookDelivery :one
-- The provider's retry reuses its webhook id, so the primary key dedups it. The
-- returned row tells a retry from a first delivery -- an unprocessed row died
-- mid-apply -- so this deliberately does not swallow the conflict.
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
-- The payload holds personal data only replay needs, so it is kept for a window
-- rather than forever. Dated from received_at when a delivery never processed:
-- the provider's retries are spent long before the window closes, so an
-- undecodable body would otherwise keep its payload for good.
delete from billing_webhook_deliveries
where coalesce(processed_at, received_at) < @older_than;

-- name: ApplyBillingSubscription :execrows
-- The mirror write: one statement, three callers. CAS on provider_updated_at, when
-- a payload ARRIVED, so this orders a delivery that overtakes another. org_id is
-- never updated: attribution is decided once, on first sight.
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
-- A tie is not hypothetical: a webhook's stamp is the whole-second
-- webhook-timestamp header, so a cutover's cancellation and activation can share
-- one. On a tie an equal stamp can end a subscription but never revive one.
where billing_subscriptions.provider_updated_at < excluded.provider_updated_at
   or (billing_subscriptions.provider_updated_at = excluded.provider_updated_at
       and billing_subscriptions.status in ('active', 'past_due'));

-- name: CreateBillingCheckoutSession :exec
-- Written before the provider is called, because the ref has to be in the
-- checkout's metadata. An abandoned checkout's row is pruned.
insert into billing_checkout_sessions (org_id, provider, ref)
values (@org_id, @provider, @ref);

-- name: GetBillingCheckoutSessionOrgID :one
-- Attribution: turns a ref that came back on a delivery into the org pug chose
-- when it started the checkout.
select org_id from billing_checkout_sessions
where provider = @provider and ref = @ref;

-- name: PruneBillingCheckoutSessions :execrows
-- A ref only has to outlive the gap between a checkout and its first delivery.
delete from billing_checkout_sessions where create_time < @older_than;

-- name: GetBillingEntitlementProviderProductID :one
-- The product an operator staged this org to buy. It is what lets a payment
-- link's metadata.org_id attribute: buyer-settable on its own, it only counts
-- when an operator has already pointed this org at this product.
select provider_product_id from billing_entitlements where org_id = @org_id;

-- name: GetBillingSubscriptionPlanSlug :one
-- Read inside the apply lock so a delivery that ENDS a subscription keeps the
-- stored slug: a product dropped from config must not refuse a cancellation.
select plan_slug from billing_subscriptions
where provider = @provider and provider_sub_id = @provider_sub_id;

-- name: ListBillingSubscriptionOrgsByProviderCustomerID :many
-- Attribution's last resort, once the ref missed and no staged product matched.
-- Two rows is the answer that matters: one buyer purchasing for two orgs shares a
-- provider customer, so the caller rejects the delivery rather than guessing.
select distinct org_id from billing_subscriptions
where provider = @provider and provider_customer_id = @provider_customer_id
limit 2;
