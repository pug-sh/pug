-- name: GetComplianceRequestByID :one
select * from compliance_requests
where id = @id and project_id = @project_id;

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
