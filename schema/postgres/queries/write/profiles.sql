-- name: SoftDeleteProfileByIDAndProjectID :execrows
update profiles
set deletion_time = now()
where id = @id and project_id = @project_id and deletion_time is null;

-- name: MergeProfileProperties :one
update profiles
set properties = jsonb_shallow_merge(s.properties, profiles.properties)
from profiles s
where s.id = @source_id
  and s.project_id = @project_id
  and s.deletion_time is null
  and profiles.id = @target_id
  and profiles.project_id = @project_id
  and profiles.deletion_time is null
returning profiles.*;

-- name: ReassignProfileDevices :exec
update profile_devices
set profile_id = @target_id
where profile_id = @source_id and project_id = @project_id;

-- name: RegisterProfile :one
-- Used only by event ingestion for anonymous profiles. The (id, project_id) conflict
-- target is not partial — it matches soft-deleted rows too. This is acceptable because
-- xid-generated IDs never collide, and this query is not used for identified profiles.
insert into profiles (properties, id, project_id)
values (coalesce(@properties::jsonb, '{}'), @id, @project_id)
on conflict (id, project_id) do update set
  properties = jsonb_shallow_merge(profiles.properties, excluded.properties)
returning *;

-- name: UpsertProfileByExternalID :one
insert into profiles (id, project_id, external_id, properties)
values (@id, @project_id, @external_id, coalesce(@properties::jsonb, '{}'))
on conflict (project_id, external_id) where deletion_time is null do update set
  properties = jsonb_shallow_merge(profiles.properties, excluded.properties)
returning *;

-- name: HardDeleteProfileByIDAndProjectID :execrows
-- Permanent erasure (GDPR/DPDP). Used only by the compliance worker; the profile's
-- properties jsonb can hold PII, so the row is physically removed, not soft-deleted.
delete from profiles
where id = @id and project_id = @project_id;
