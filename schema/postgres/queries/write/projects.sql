-- name: CreateProject :one
insert into projects (display_name, id, org_id, private_api_key, public_api_key)
values (@display_name, @id, @org_id, @private_api_key, @public_api_key)
returning *;

-- name: CreateProjectAsAdmin :one
with check_admin as (
  select 1 from org_members
  where org_id = @org_id and customer_id = @customer_id and role = 'ORG_ROLE_ADMIN'
)
insert into projects (display_name, id, org_id, private_api_key, public_api_key)
select @display_name, @id, @org_id, @private_api_key, @public_api_key
where exists (select 1 from check_admin)
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
