-- name: CreateCustomer :one
insert into customers (id, display_name, email, password_hash, picture_uri)
values (@id, @display_name, @email, @password_hash, @picture_uri)
returning *;

-- name: MarkCustomerEmailVerified :one
update customers
set email_verified_at = now()
where id = @id
returning *;

-- name: UpdateCustomerPasswordHash :one
update customers
set password_hash = @password_hash
where id = @id
returning *;
