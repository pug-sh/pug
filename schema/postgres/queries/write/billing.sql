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
  included_events_override, note, org_id, plan_slug, trial_ends_at
) values (
  @anchor_day, @contract_ends_at, @display_name_override,
  @included_events_override, @note, @org_id, @plan_slug, @trial_ends_at
)
on conflict (org_id) do update
set anchor_day = excluded.anchor_day,
    contract_ends_at = excluded.contract_ends_at,
    display_name_override = excluded.display_name_override,
    included_events_override = excluded.included_events_override,
    note = excluded.note,
    plan_slug = excluded.plan_slug,
    trial_ends_at = excluded.trial_ends_at
returning *;

-- name: DeleteBillingEntitlement :execrows
delete from billing_entitlements where org_id = @org_id;

-- name: InsertBillingEntitlementHistory :exec
insert into billing_entitlement_history (
  actor, anchor_day, contract_ends_at, display_name_override,
  id, included_events_override, note, org_id, plan_slug, trial_ends_at
) values (
  @actor, @anchor_day, @contract_ends_at, @display_name_override,
  @id, @included_events_override, @note, @org_id, @plan_slug, @trial_ends_at
);
