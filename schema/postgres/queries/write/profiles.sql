-- name: DeleteProfileByIDAndProjectID :execrows
delete from profiles
where id = @id and project_id = @project_id;

-- name: MergeProfileProperties :one
update profiles
set properties = jsonb_shallow_merge(s.properties, profiles.properties)
from profiles s
where s.id = @source_id
  and s.project_id = @project_id
  and profiles.id = @target_id
  and profiles.project_id = @project_id
returning profiles.*;

-- name: ReassignProfileDevices :exec
update profile_devices
set profile_id = @target_id
where profile_id = @source_id and project_id = @project_id;

-- name: RegisterProfile :one
insert into profiles (properties, id, project_id)
values (coalesce(@properties::jsonb, '{}'), @id, @project_id)
on conflict (id, project_id) do update set
  properties = jsonb_shallow_merge(profiles.properties, excluded.properties)
returning *;

-- name: UpsertProfileByExternalID :one
insert into profiles (id, project_id, external_id, properties)
values (@id, @project_id, @external_id, coalesce(@properties::jsonb, '{}'))
on conflict (project_id, external_id) do update set
  properties = jsonb_shallow_merge(profiles.properties, excluded.properties)
returning *;
