-- name: UpsertCustomer :one
insert into customers (display_name, email, id, picture_uri)
values (@display_name, @email, @id, @picture_uri) on conflict (email) do
update -- do not update id
set display_name = excluded.display_name,
  picture_uri = excluded.picture_uri
returning *;
