-- name: GetProjectsByOrgID :many
select p.* from projects p join orgs o on o.id=p.org_id where p.org_id = @org_id and p.deletion_state='active' and o.deletion_state='active' order by p.create_time asc, p.id asc;

-- name: GetProjectByID :one
select p.* from projects p join orgs o on o.id=p.org_id where p.id = @id and p.deletion_state='active' and o.deletion_state='active';

-- name: GetProjectByIDAndOrgMember :one
select p.*
from projects p
join org_members om on om.org_id = p.org_id
join orgs o on o.id = p.org_id
where p.id = @id and om.customer_id = @customer_id and p.deletion_state='active' and o.deletion_state='active';

-- name: GetProjectByPrivateApiKey :one
-- @token is the sha256 hex of the presented prv_ key — private keys are stored
-- hashed, so the caller hashes before looking up (see core/projects.hashKey).
select p.*
from projects p
join api_keys k on k.project_id = p.id
join orgs o on o.id = p.org_id
where k.token = @token and k.kind = 'private' and p.deletion_state='active' and o.deletion_state='active';

-- name: GetProjectByPublicApiKey :one
-- @token is the pub_ key itself — public keys are stored plaintext.
select p.*
from projects p
join api_keys k on k.project_id = p.id
join orgs o on o.id = p.org_id
where k.token = @token and k.kind = 'public' and p.deletion_state='active' and o.deletion_state='active';

