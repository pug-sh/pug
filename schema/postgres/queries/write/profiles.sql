-- name: CreateProfile :one
insert into profiles (id, project_id, external_id, properties, custom_properties)
values (@id, @project_id, @external_id, coalesce(@properties, '{}'), coalesce(@custom_properties, '{}'))
returning *;

-- name: UpdateProfileProperties :one
update profiles
set properties = coalesce(@properties, '{}'), update_time = now()
where id = @id and project_id = @project_id
returning *;

-- name: UpdateProfileCustomProperties :one
update profiles
set custom_properties = coalesce(@custom_properties, '{}'), update_time = now()
where id = @id and project_id = @project_id
returning *;

-- name: DeleteProfileByIDAndProjectID :exec
delete from profiles
where id = @id and project_id = @project_id;
