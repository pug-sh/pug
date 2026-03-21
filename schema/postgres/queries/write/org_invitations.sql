-- name: GetOrgInvitationByTokenForUpdate :one
select * from org_invitations where token = @token for update;

-- name: CreateOrgInvitation :one
with check_member as (
  select 1 from org_members om
  join customers c on c.id = om.customer_id
  where om.org_id = @org_id and lower(c.email) = lower(@email)
)
insert into org_invitations (email, expires_at, id, inviter_id, org_id, token)
select @email, @expires_at, @id, @inviter_id, @org_id, @token
where not exists (select 1 from check_member)
returning *;

-- name: UpdateOrgInvitationStatus :one
update org_invitations set status = @status where id = @id
returning *;
