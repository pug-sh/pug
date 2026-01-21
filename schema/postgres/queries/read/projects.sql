-- name: GetProjectsByCustomerId :many
select *
from projects
where customer_id = @customer_id;

-- name: GetProjectById :one
select *
from projects
where id = @id;

-- name: ProjectExistsForCustomer :one
select exists(
  select 1
  from projects
  where id = @id and customer_id = @customer_id
);

-- name: GetProjectAndCustomerByApiKey :one
select sqlc.embed(projects), sqlc.embed(customers)
from projects
join customers on customers.id = projects.customer_id
where projects.api_key = @api_key;

-- name: GetProjectByIDAndCustomerID :one
select * from projects where id = @id and customer_id = @customer_id;
