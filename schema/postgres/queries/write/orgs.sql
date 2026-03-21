-- name: CreateOrg :one
insert into orgs (display_name, id)
values (@display_name, @id)
returning *;

-- name: GetOrgByID :one
select * from orgs where id = @id;

-- name: UpdateOrgDisplayName :one
update orgs set display_name = @display_name where id = @id
returning *;
