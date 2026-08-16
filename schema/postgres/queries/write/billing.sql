-- name: LockBillingEntitlementOrg :exec
-- Serializes an org's entitlement writes. GetBillingEntitlementForUpdate below
-- locks nothing when the org has no row yet, so without this two concurrent
-- first writes both read an empty record and the second full-replaces the first.
select pg_advisory_xact_lock(hashtext('billing_entitlement:' || @org_id::text));

-- name: GetBillingEntitlementForUpdate :one
-- Locks the row for a read-modify-write: the CLI leaves un-passed flags at their
-- stored values, so it has to read what is there before it can write what should
-- be. Returns no rows for an org that has never been touched, which is normal.
select * from billing_entitlements where org_id = @org_id for update;

-- name: UpsertBillingEntitlement :one
-- Full replace, never partial: the caller has already merged its flags over the
-- locked row, so a coalesce here would be a second, disagreeing merge.
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
-- Append-only, written in the same transaction as the change it records. NULL
-- across the value columns is a deletion.
insert into billing_entitlement_history (
  actor, anchor_day, contract_ends_at, display_name_override,
  id, included_events_override, note, org_id, plan_slug, trial_ends_at
) values (
  @actor, @anchor_day, @contract_ends_at, @display_name_override,
  @id, @included_events_override, @note, @org_id, @plan_slug, @trial_ends_at
);
