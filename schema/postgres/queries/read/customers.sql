-- name: GetCustomerByID :one
select *
from customers
where id = @id;

-- name: GetCustomerByEmail :one
select *
from customers
where email = @email;

-- name: GetCustomerByEmailOptional :one
select *
from customers
where lower(email) = lower(@email);
