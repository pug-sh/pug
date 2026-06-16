-- name: CreateComplianceRequest :one
insert into compliance_requests (id, project_id, kind, profile_id, external_id, requested_by)
values (@id, @project_id, @kind, @profile_id, @external_id, @requested_by)
returning *;

-- name: FreezeComplianceRequestIdentifiers :execrows
-- Erase-only: records the resolved distinct_ids + session_ids (and event count)
-- and advances the request to 'processing'. Frozen once so worker retries reuse
-- the same set.
update compliance_requests
set distinct_ids    = @distinct_ids,
    session_ids     = @session_ids,
    events_affected = @events_affected,
    status          = 'processing'
where id = @id and project_id = @project_id;

-- name: MarkComplianceRequestCompleted :execrows
update compliance_requests
set status = 'completed', completed_at = now()
where id = @id and project_id = @project_id;

-- name: MarkComplianceRequestFailed :execrows
-- Records a permanent erasure failure on the audit row so the DSAR ledger
-- reflects reality instead of a request stuck at 'processing'. Set by the worker
-- just before a message is dead-lettered.
update compliance_requests
set status = 'failed', error = @error
where id = @id and project_id = @project_id;

-- name: ReopenComplianceRequest :execrows
-- Revives a non-completed erase request so a retry re-drives the same row:
-- clears any prior error and resets status to 'pending'. Never touches a
-- 'completed' row. Frozen distinct_ids/session_ids are left intact so the
-- re-driven worker pass reuses them.
update compliance_requests
set status = 'pending', error = null
where id = @id and project_id = @project_id and status <> 'completed';
