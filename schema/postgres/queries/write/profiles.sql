-- name: DeleteProfileByIDAndProjectID :exec
delete from profiles
where id = @id and project_id = @project_id;

-- name: MergeProfileProperties :one
update profiles
set auto_properties = jsonb_shallow_merge(s.auto_properties, profiles.auto_properties),
    custom_properties = jsonb_shallow_merge(s.custom_properties, profiles.custom_properties)
from profiles s
where s.id = @source_id
  and s.project_id = @project_id
  and profiles.id = @target_id
  and profiles.project_id = @project_id
returning profiles.*;

-- name: GetProfileByProjectAndExternalID :one
select * from profiles
where project_id = @project_id and external_id = @external_id limit 1;

-- name: ReassignProfileSubscriptions :exec
update subscriptions
set profile_id = @target_id
where profile_id = @source_id and project_id = @project_id;

-- name: SetProfileExternalID :one
update profiles
set external_id = @external_id
where id = @id and project_id = @project_id
returning *;

-- name: SaveProfile :one
insert into profiles (auto_properties, custom_properties, external_id, id, project_id)
values (coalesce(@auto_properties, '{}'), coalesce(@custom_properties, '{}'), @external_id, @id, @project_id)
on conflict (project_id, external_id) do update set
  auto_properties = jsonb_shallow_merge(profiles.auto_properties, excluded.auto_properties),
  custom_properties = jsonb_shallow_merge(profiles.custom_properties, excluded.custom_properties)
returning *;
