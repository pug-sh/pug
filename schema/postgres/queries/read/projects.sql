-- name: GetProjectsByOrgID :many
select * from projects where org_id = @org_id order by create_time asc, id asc;

-- name: GetProjectByID :one
select * from projects where id = @id;

-- name: GetProjectByIDAndOrgMember :one
select p.*
from projects p
join org_members om on om.org_id = p.org_id
where p.id = @id and om.customer_id = @customer_id;

-- name: GetProjectByPrivateApiKey :one
-- @token is the sha256 hex of the presented prv_ key — private keys are stored
-- hashed, so the caller hashes before looking up (see core/projects.hashKey).
select p.*
from projects p
join api_keys k on k.project_id = p.id
where k.token = @token and k.kind = 'private';

-- name: GetProjectByPublicApiKey :one
-- @token is the pub_ key itself — public keys are stored plaintext.
select p.*
from projects p
join api_keys k on k.project_id = p.id
where k.token = @token and k.kind = 'public';

