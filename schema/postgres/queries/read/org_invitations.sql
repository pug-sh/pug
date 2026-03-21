-- name: GetOrgInvitationByToken :one
select * from org_invitations where token = @token;

-- name: GetOrgInvitationsByOrgID :many
select * from org_invitations
where org_id = @org_id
order by create_time desc;
