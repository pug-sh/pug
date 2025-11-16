-- name: CreateUser :one
insert into users (id, project_id, external_id, metadata)
values (@id, @project_id, @external_id, @metadata)
returning *;

-- name: UpdateUserMetadata :one
update users
set metadata = @metadata, update_time = now()
where id = @id
returning *;

-- name: DeleteUser :exec
delete from users
where id = @id;
