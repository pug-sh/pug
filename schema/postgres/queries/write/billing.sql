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
-- provider_product_id is not written here any more: every org authorizes against
-- the one mandate product, and migration 022 drops the column.
insert into billing_entitlements (
  anchor_day, contract_ends_at, display_name_override, flat_fee_cents,
  included_events_override, note, org_id, plan_slug, rate_cents_per_million,
  retention_days_override, terms_effective_at, trial_ends_at
) values (
  @anchor_day, @contract_ends_at, @display_name_override, @flat_fee_cents,
  @included_events_override, @note, @org_id, @plan_slug, @rate_cents_per_million,
  @retention_days_override, @terms_effective_at, @trial_ends_at
)
on conflict (org_id) do update
set anchor_day = excluded.anchor_day,
    contract_ends_at = excluded.contract_ends_at,
    display_name_override = excluded.display_name_override,
    flat_fee_cents = excluded.flat_fee_cents,
    included_events_override = excluded.included_events_override,
    note = excluded.note,
    plan_slug = excluded.plan_slug,
    provider_product_id = null,
    rate_cents_per_million = excluded.rate_cents_per_million,
    retention_days_override = excluded.retention_days_override,
    terms_effective_at = excluded.terms_effective_at,
    trial_ends_at = excluded.trial_ends_at
returning *;

-- name: DeleteBillingEntitlement :execrows
delete from billing_entitlements where org_id = @org_id;

-- name: InsertBillingEntitlementHistory :exec
insert into billing_entitlement_history (
  actor, anchor_day, contract_ends_at, deleted, display_name_override,
  flat_fee_cents, id, included_events_override, note, org_id, plan_slug,
  rate_cents_per_million, retention_days_override, terms_effective_at, trial_ends_at
) values (
  @actor, @anchor_day, @contract_ends_at, @deleted, @display_name_override,
  @flat_fee_cents, @id, @included_events_override, @note, @org_id, @plan_slug,
  @rate_cents_per_million, @retention_days_override, @terms_effective_at, @trial_ends_at
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
  cancel_at_period_end, currency, current_period_end, current_period_start,
  ended_at, id, on_demand, org_id, plan_slug, provider, provider_customer_id,
  provider_status, provider_sub_id, provider_updated_at, status
) values (
  @cancel_at_period_end, @currency, @current_period_end, @current_period_start,
  @ended_at, @id, @on_demand, @org_id, @plan_slug, @provider, @provider_customer_id,
  @provider_status, @provider_sub_id, @provider_updated_at, @status
)
on conflict (provider, provider_sub_id) do update
set cancel_at_period_end = excluded.cancel_at_period_end,
    currency = excluded.currency,
    current_period_end = excluded.current_period_end,
    current_period_start = excluded.current_period_start,
    -- Once ended, only an earlier date moves it: a later read dated when pug saw it
    -- must not stretch the days a close bills.
    ended_at = case
      when billing_subscriptions.status in ('active', 'past_due')
        or excluded.status in ('active', 'past_due') then excluded.ended_at
      else least(billing_subscriptions.ended_at, excluded.ended_at)
    end,
    on_demand = excluded.on_demand,
    plan_slug = excluded.plan_slug,
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
insert into billing_checkout_sessions (org_id, plan_slug, provider, ref)
values (@org_id, @plan_slug, @provider, @ref);

-- name: GetBillingCheckoutSession :one
-- Attribution: turns a ref that came back on a delivery into the org pug chose
-- when it started the checkout, and the card it pinned.
select org_id, plan_slug from billing_checkout_sessions
where provider = @provider and ref = @ref;

-- name: PruneBillingCheckoutSessions :execrows
-- A ref only has to outlive the gap between a checkout and its first delivery.
delete from billing_checkout_sessions where create_time < @older_than;

-- name: GetBillingSubscriptionPlanSlug :one
-- The pinned card, read inside the apply lock so a delivery carrying no ref keeps
-- the card the checkout pinned rather than resolving one afresh.
select plan_slug from billing_subscriptions
where provider = @provider and provider_sub_id = @provider_sub_id;

-- name: ListBillingSubscriptionOrgsByProviderCustomerID :many
-- Attribution's last resort, once the ref missed and no staged product matched.
-- Two rows is the answer that matters: one buyer purchasing for two orgs shares a
-- provider customer, so the caller rejects the delivery rather than guessing.
select distinct org_id from billing_subscriptions
where provider = @provider and provider_customer_id = @provider_customer_id
limit 2;

-- name: InsertBillingInvoice :one
-- The unique (org, billed_from) key is the guard against two passes closing one
-- window: the loser inserts nothing and reads no row.
insert into billing_invoices (
  amount_cents, billed_from, billed_to, carried_cents, currency, event_count, id, lines,
  next_attempt_at, org_id, period_end, period_start, plan_slug, pricing, status,
  usage_cents, usage_computed_at
) values (
  @amount_cents, @billed_from, @billed_to, @carried_cents, @currency, @event_count, @id, @lines,
  @next_attempt_at, @org_id, @period_end, @period_start, @plan_slug, @pricing, @status,
  @usage_cents, @usage_computed_at
)
on conflict (org_id, billed_from) do nothing
returning *;

-- name: InsertBillingInvoiceEvent :exec
insert into billing_invoice_events (actor, detail, from_status, id, invoice_id, to_status)
values (@actor, @detail, @from_status, @id, @invoice_id, @to_status);

-- name: LockUncoveredDeferredBillingInvoices :many
-- The balance a close carries, locked so a concurrent close cannot carry or sweep
-- the same rows.
select id, period_start, usage_cents from billing_invoices
where org_id = @org_id and status = 'deferred' and covered_by is null
order by period_start
for update;

-- name: CoverBillingInvoices :execrows
update billing_invoices set covered_by = @covered_by
where id = any(@ids::text[]) and status = 'deferred' and covered_by is null;

-- name: WaiveDeferredBillingInvoices :execrows
update billing_invoices set status = 'waived'
where id = any(@ids::text[]) and status = 'deferred' and covered_by is null;

-- name: GetBillingInvoiceStatusForUpdate :one
select status from billing_invoices where id = @id for update;

-- name: MarkBillingInvoiceCharging :one
-- The intent, committed before the provider is called. Each attempt stamps the
-- mandate it charges and clears what the previous attempt left.
update billing_invoices
set status = 'charging', provider = @provider, provider_sub_id = @provider_sub_id,
    provider_payment_id = null, last_error_code = '', last_error_message = ''
where id = @id and status in ('open', 'failed')
returning *;

-- name: MarkBillingInvoiceCharged :one
update billing_invoices
set status = 'charged', attempts = attempts + 1, provider_payment_id = @provider_payment_id
where id = @id and status = 'charging'
returning *;

-- name: MarkBillingInvoiceFailed :one
update billing_invoices
set status = 'failed', attempts = attempts + 1, failed_at = @failed_at, next_attempt_at = null,
    last_error_code = @last_error_code, last_error_message = @last_error_message
where id = @id and status = 'charging'
returning *;

-- name: MarkBillingInvoiceUncollectible :one
update billing_invoices
set status = 'uncollectible', failed_at = @failed_at, next_attempt_at = null,
    last_error_code = @last_error_code, last_error_message = @last_error_message
where id = @id and status in ('open', 'charging', 'failed')
returning *;

-- name: RecordBillingInvoiceChargeError :execrows
-- A code on a charging row is a refusal that took nothing; an unknown outcome
-- keeps only its message.
update billing_invoices
set last_error_code = @last_error_code, last_error_message = @last_error_message
where id = @id and status = 'charging';
