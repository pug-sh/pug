-- name: StartProjectDeletion :execrows
update project_deletions set status = 'deleting'
where project_id = @project_id and status = 'pending';

-- name: RecordProjectDeletionRound :exec
update project_deletions
set rounds = rounds + 1, round_started_at = @round_started_at
where project_id = @project_id;

-- name: SetProjectDeletionError :exec
update project_deletions set error = @error where project_id = @project_id;

-- name: FinishProjectDeletion :execrows
update project_deletions set status = 'done', done_at = @done_at
where project_id = @project_id and status = 'deleting';

-- name: ReopenProjectDeletion :execrows
update project_deletions set status = 'deleting', done_at = null
where project_id = @project_id and status = 'done';

-- name: DeleteProfileDevicesBatch :execrows
delete from profile_devices d
where d.project_id = @project_id
  and d.id in (select b.id from profile_devices b where b.project_id = @project_id limit @row_limit);

-- name: DeleteProfilesBatch :execrows
delete from profiles p
where p.project_id = @project_id
  and p.id in (select b.id from profiles b where b.project_id = @project_id limit @row_limit);

-- name: DeleteHiddenProject :execrows
-- Cascades whatever the batches left. A live project never matches.
delete from projects where id = @id and deletion_time is not null;
