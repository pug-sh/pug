-- name: GetProjectsByOrgID :many
select * from projects where org_id = @org_id order by create_time asc;

-- name: GetProjectByID :one
select * from projects where id = @id;

-- name: GetProjectByIDAndOrgMember :one
select p.*
from projects p
join org_members om on om.org_id = p.org_id
where p.id = @id and om.customer_id = @customer_id;

-- name: GetProjectByPrivateApiKey :one
select * from projects where private_api_key = @private_api_key;

-- name: GetProjectByPublicApiKey :one
select * from projects where public_api_key = @public_api_key;
