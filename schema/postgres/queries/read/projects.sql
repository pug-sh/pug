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
-- name: GetProjectByApiKey :one

select *
from projects
where api_key = @api_key;
