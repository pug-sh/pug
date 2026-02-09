-- name: CreateProfile :one
insert into profiles (auto_properties, custom_properties, external_id, id, project_id)
values (coalesce(@auto_properties, '{}'), coalesce(@custom_properties, '{}'), @external_id, @id, @project_id)
returning *;

-- name: DeleteProfileByIDAndProjectID :exec
delete from profiles
where id = @id and project_id = @project_id;

-- name: SaveProfile :one
insert into profiles (auto_properties, custom_properties, external_id, id, project_id)
values (coalesce(@auto_properties, '{}'), coalesce(@custom_properties, '{}'), @external_id, @id, @project_id)
on conflict (project_id, external_id) do update set
  auto_properties = jsonb_deep_merge(profiles.auto_properties, excluded.auto_properties),
  custom_properties = jsonb_deep_merge(profiles.custom_properties, excluded.custom_properties)
returning *;

-- name: UpdateProfileAutoProperties :one
update profiles
set auto_properties = coalesce(@auto_properties, '{}')
where id = @id and project_id = @project_id
returning *;

-- name: UpdateProfileCustomProperties :one
update profiles
set custom_properties = coalesce(@custom_properties, '{}')
where id = @id and project_id = @project_id
returning *;
