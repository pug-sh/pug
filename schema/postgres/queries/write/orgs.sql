-- name: CreateOrg :one
insert into orgs (display_name, id)
values (@display_name, @id)
returning *;

-- name: GetOrgByID :one
select * from orgs where id = @id;

-- name: UpdateOrgDisplayName :one
update orgs set display_name = @display_name where id = @id
returning *;

-- name: GetOrgByIDForUpdate :one
select * from orgs where id = @id for update;

-- name: UpdateOrgDomainSettings :one
update orgs
set auto_join_role = @auto_join_role,
    members_can_create_orgs = @members_can_create_orgs
where id = @id
returning *;
