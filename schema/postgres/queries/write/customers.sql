-- name: CreateCustomer :one
insert into customers (id, display_name, email, password_hash, picture_uri)
values (@id, @display_name, @email, @password_hash, @picture_uri)
returning *;

-- name: UpsertCustomer :one
insert into customers (display_name, email, id, picture_uri, password_hash)
values (@display_name, @email, @id, @picture_uri, @password_hash) on conflict (email) do
update -- do not update id
set display_name = excluded.display_name,
  picture_uri = excluded.picture_uri,
  password_hash = excluded.password_hash
returning *;
