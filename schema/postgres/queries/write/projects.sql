-- name: CreateProject :one
insert into projects (api_key, customer_id, display_name, fcm_service_json, id)
values (@api_key, @customer_id, @display_name, @fcm_service_json, @id)
returning *;

-- name: DeleteProject :one
delete from projects
where customer_id = @customer_id and id = @id
returning *;

-- name: UpdateFCMServiceJSON :one
update projects
set fcm_service_json = @fcm_service_json
where customer_id = @customer_id and id = @id
returning *;

-- name: UpdateProjectDisplayName :one
update projects
set display_name = @display_name
where customer_id = @customer_id and id = @id
returning *;
