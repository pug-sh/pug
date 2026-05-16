-- name: GetOrgInvitationByToken :one
select * from org_invitations where token = @token;

-- name: GetOrgInvitationsByOrgID :many
select * from org_invitations
where org_id = @org_id
order by create_time desc;

-- name: GetOrgInvitationEmailContextByID :one
select
  oi.id,
  oi.email,
  oi.org_id,
  o.display_name as org_display_name,
  coalesce(c.display_name, '') as inviter_display_name
from org_invitations oi
join orgs o on o.id = oi.org_id
left join customers c on c.id = oi.inviter_id
where oi.id = @id;
