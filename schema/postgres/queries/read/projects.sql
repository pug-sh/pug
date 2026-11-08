-- name: GetProjectsByOrgID :many
select * from projects
where org_id = @org_id and deletion_time is null
order by create_time asc, id asc;

-- name: GetProjectByID :one
select * from projects where id = @id and deletion_time is null;

-- name: GetProjectByIDAndOrgMember :one
select p.*
from projects p
join org_members om on om.org_id = p.org_id
where p.id = @id and om.customer_id = @customer_id and p.deletion_time is null;

-- name: GetProjectByPrivateApiKey :one
-- @token is the sha256 hex of the presented prv_ key: private keys are stored
-- hashed, so the caller hashes before looking up (see core/projects.hashKey).
select p.*
from projects p
join api_keys k on k.project_id = p.id
where k.token = @token and k.kind = 'private' and p.deletion_time is null;

-- name: GetProjectByPublicApiKey :one
-- @token is the pub_ key itself: public keys are stored plaintext.
select p.*
from projects p
join api_keys k on k.project_id = p.id
where k.token = @token and k.kind = 'public' and p.deletion_time is null;

-- name: ListLiveProjects :many
-- Every project retention cuts, with the org whose length it follows.
select id, org_id from projects where deletion_time is null order by id;

-- name: GetProjectsWithOrgByCustomerID :many
-- Every live project in every org the customer belongs to, with any role.
select
  o.display_name as org_display_name,
  p.display_name,
  p.id,
  p.org_id
from projects p
join orgs o on o.id = p.org_id
join org_members om on om.org_id = p.org_id
where om.customer_id = @customer_id
  and p.deletion_time is null
order by o.create_time asc, o.id asc, p.create_time asc, p.id asc;
