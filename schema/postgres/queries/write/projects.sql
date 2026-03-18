-- name: CreateProject :one
insert into projects (created_by, display_name, id, org_id, private_api_key, public_api_key)
values (@created_by, @display_name, @id, @org_id, @private_api_key, @public_api_key)
returning *;

-- name: DeleteProject :one
delete from projects
where org_id = @org_id and id = @id
returning *;

-- name: UpdateFCMServiceJSON :one
update projects
set fcm_service_json = @fcm_service_json
where org_id = @org_id and id = @id
returning *;

-- name: UpdateProjectDisplayName :one
update projects
set display_name = @display_name
where org_id = @org_id and id = @id
returning *;
