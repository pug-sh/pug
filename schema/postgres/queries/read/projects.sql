-- name: GetProjectsByCustomerID :many
select *
from projects
where customer_id = @customer_id;

-- name: GetProjectByID :one
select *
from projects
where id = @id;

-- name: ProjectExistsForCustomer :one
select exists(
  select 1
  from projects
  where id = @id and customer_id = @customer_id
);

-- name: GetProjectAndCustomerByPrivateApiKey :one
-- NOTE: The customer join is currently unused by SDK/shared auth handlers, which only
-- access principal.Project.ID. The join exists because Principal embeds dbread.Customer.
-- If the Principal type is ever refactored to not require a Customer for API key auth,
-- this query can be simplified to select from projects only.
select sqlc.embed(projects), sqlc.embed(customers)
from projects
join customers on customers.id = projects.customer_id
where projects.private_api_key = @private_api_key;

-- name: GetProjectAndCustomerByApiKey :one
-- NOTE: Same as above — the customer join is unused by SDK auth handlers.
-- Also note: this query matches on either public_api_key OR private_api_key.
-- It is used by WithSDKAuth (accepts both keys). WithDualAuth uses only
-- GetProjectAndCustomerByPrivateApiKey and rejects public keys silently.
select sqlc.embed(projects), sqlc.embed(customers)
from projects
join customers on customers.id = projects.customer_id
where projects.public_api_key = @api_key or projects.private_api_key = @api_key;

-- name: GetProjectByIDAndCustomerID :one
select * from projects where id = @id and customer_id = @customer_id;
