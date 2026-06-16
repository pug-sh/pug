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
update compliance_requests
set status = 'failed', error = @error
where id = @id and project_id = @project_id;
