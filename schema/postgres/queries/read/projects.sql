-- name: GetProjectsByOrgID :many
select * from projects where org_id = @org_id;

-- name: GetProjectByID :one
select * from projects where id = @id;

-- name: GetProjectByIDAndOrgMember :one
select p.*
from projects p
join org_members om on om.org_id = p.org_id
where p.id = @id and om.customer_id = @customer_id;

-- name: GetProjectAndCustomerByPrivateApiKey :one
-- NOTE: The customer data from this join is required by the Principal struct populated in
-- WithDualAuth, but is not accessed by downstream shared handler code. If Principal is
-- refactored to not require a Customer for API key auth, this query can be simplified
-- to select from projects only.
select sqlc.embed(projects), sqlc.embed(customers)
from projects
join customers on customers.id = projects.created_by
where projects.private_api_key = @private_api_key;

-- name: GetProjectAndCustomerByPublicApiKey :one
-- NOTE: Same as above — the customer data is required by the Principal struct
-- populated in WithSDKAuth, but is not accessed by downstream SDK handler code.
select sqlc.embed(projects), sqlc.embed(customers)
from projects
join customers on customers.id = projects.created_by
where projects.public_api_key = @public_api_key;
