-- name: CreateOrg :one
insert into orgs (display_name, id)
values (@display_name, @id)
returning *;

-- name: UpdateOrgDisplayName :one
update orgs set display_name = @display_name where id = @id
returning *;

-- name: CreateOrgMember :one
insert into org_members (customer_id, org_id, role)
values (@customer_id, @org_id, @role)
returning *;

-- name: DeleteOrgMember :exec
delete from org_members where org_id = @org_id and customer_id = @customer_id;

-- name: CreateOrgInvitation :one
insert into org_invitations (email, expires_at, id, inviter_id, org_id, token)
values (@email, @expires_at, @id, @inviter_id, @org_id, @token)
returning *;

-- name: UpdateOrgInvitationStatus :one
update org_invitations set status = @status where id = @id
returning *;
