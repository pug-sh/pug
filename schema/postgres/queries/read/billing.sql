-- name: GetOrgEntitlement :one
-- The org's create_time (the trial clock AND the default quota anchor) plus its
-- entitlement row, which is absent for almost every org -- a left join rather
-- than two reads, since "no row" is the ordinary answer and not an error.
select
  o.create_time as org_create_time,
  e.anchor_day,
  e.contract_ends_at,
  e.display_name_override,
  e.included_events_override,
  e.note,
  e.plan_slug,
  e.trial_ends_at
from orgs o
left join billing_entitlements e on e.org_id = o.id
where o.id = @org_id;

-- name: ListBillingEntitlementHistory :many
-- Operator/support data, newest first. Not served by any RPC.
select * from billing_entitlement_history
where org_id = @org_id
order by changed_at desc, id desc
limit @row_limit;
