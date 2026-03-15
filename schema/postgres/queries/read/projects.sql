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
-- NOTE: The customer join is currently unused by shared auth handlers (WithDualAuth), which only
-- access principal.Project.ID. The join exists because Principal embeds dbread.Customer.
-- If the Principal type is ever refactored to not require a Customer for API key auth,
-- this query can be simplified to select from projects only.
select sqlc.embed(projects), sqlc.embed(customers)
from projects
join customers on customers.id = projects.customer_id
where projects.private_api_key = @private_api_key;

-- name: GetProjectAndCustomerByPublicApiKey :one
-- NOTE: Same as above — the customer join is unused by SDK auth handlers.
-- Used by WithSDKAuth which only accepts public keys.
select sqlc.embed(projects), sqlc.embed(customers)
from projects
join customers on customers.id = projects.customer_id
where projects.public_api_key = @public_api_key;

-- name: GetProjectByIDAndCustomerID :one
select * from projects where id = @id and customer_id = @customer_id;
