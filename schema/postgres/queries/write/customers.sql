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

-- name: GetCustomerSignInState :one
select disabled_at, session_version from customers where id = @id;

-- name: SetCustomerDisabled :one
update customers
set disabled_at = case when @disabled::boolean then now() else null end
where id = @id
returning *;

-- name: BumpCustomerSessionVersion :execrows
update customers set session_version = session_version + 1 where id = @id;
