-- name: GetCustomerByID :one
select *
from customers
where id = @id;

-- name: GetCustomerByEmail :one
select *
from customers
where email = @email;
