-- name: CreateUser :one
insert into users (id, project_id, external_id, properties, custom_properties)
values (@id, @project_id, @external_id, @properties, @custom_properties)
returning *;

-- name: UpdateUserProperties :one
update users
set properties = @properties, update_time = now()
where id = @id and project_id = @project_id
returning *;

-- name: UpdateUserCustomProperties :one
update users
set custom_properties = @custom_properties, update_time = now()
where id = @id and project_id = @project_id
returning *;

-- name: DeleteUserByIDAndProjectID :exec
delete from users
where id = @id and project_id = @project_id;
