-- name: UpsertRetentionState :exec
insert into retention_state (days, expired_by, org_id, pending_days, pending_since)
values (@days, @expired_by, @org_id, @pending_days, @pending_since)
on conflict (org_id) do update set
  days = excluded.days,
  expired_by = excluded.expired_by,
  pending_days = excluded.pending_days,
  pending_since = excluded.pending_since;

-- name: ExpireRetentionState :one
-- Starts the wait on a drop held for an operator. No row means nothing waits.
update retention_state
set expired_by = @expired_by, pending_since = @pending_since
where org_id = @org_id and pending_days is not null and pending_since is null
returning *;
