-- name: GetOrgInvitationByTokenForUpdate :one
select * from org_invitations where token = @token for update;

-- name: CreateOrgInvitation :one
insert into org_invitations (email, expires_at, id, inviter_id, org_id, token)
values (@email, @expires_at, @id, @inviter_id, @org_id, @token)
returning *;

-- name: UpdateOrgInvitationStatus :one
update org_invitations set status = @status where id = @id
returning *;
