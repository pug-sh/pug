-- name: CreateCustomer :one
insert into customers (id, display_name, email, password_hash, picture_uri)
values (@id, @display_name, @email, @password_hash, @picture_uri)
returning *;
