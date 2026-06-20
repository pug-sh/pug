-- name: GetEraseRequestByID :one
-- Erase-path load. Scoped to kind = 'erase' so the erasure executor and the
-- erasure-status RPC structurally cannot load (and the executor cannot hard-delete
-- against) an 'export' row once the unified ledger holds both kinds — an export row
-- simply reads as not-found here. Export gets its own GetExportRequestByID.
select * from compliance_requests
where id = @id and project_id = @project_id and kind = 'erase';

-- name: ListStuckComplianceRequests :many
-- Operability / SLA backstop: open (pending|processing) requests whose enqueue may
-- have been lost (publish failure) or aged out of the compliance stream (max_age 720h)
-- with nothing to re-drive them. Surfaced to oncall/alerting so an operator can
-- re-drive via a fresh RequestErasure* call (frozen identifiers keep the re-drive
-- correct). Bounded by @row_limit so the alert query stays cheap.
select * from compliance_requests
where status in ('pending', 'processing')
  and requested_at < @older_than::timestamptz
order by requested_at asc
limit @row_limit;

-- name: GetReopenableComplianceRequest :one
-- Idempotency / re-drive: the most recent non-completed request for a data
-- subject, matched by whichever identifier the caller holds. Lets a retried
-- erasure re-drive the existing ledger row (reviving a 'failed' one) instead of
-- inserting a duplicate, and re-publish an enqueue that never reached the worker.
-- A 'completed' request is excluded so a genuinely new erasure (e.g. data
-- re-arrived under the same id) starts a fresh row.
select * from compliance_requests
where project_id = @project_id
  and kind = @kind
  and status <> 'completed'
  and (
    (@profile_id::text <> '' and profile_id::text = @profile_id::text)
    or (@external_id::text <> '' and external_id = @external_id::text)
  )
order by requested_at desc
limit 1;
