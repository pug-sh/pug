-- name: GetOrgByID :one
select * from orgs where id = @id;

-- name: GetOrgsByCustomerID :many
select o.*
from orgs o
join org_members om on om.org_id = o.id
where om.customer_id = @customer_id
order by o.create_time asc;

-- name: IsOrgMember :one
select exists(
  select 1 from org_members
  where org_id = @org_id and customer_id = @customer_id
);

-- name: GetOrgMembersByOrgID :many
select
  om.customer_id,
  om.create_time,
  om.org_id,
  om.role,
  c.display_name,
  c.email
from org_members om
join customers c on c.id = om.customer_id
where om.org_id = @org_id
order by om.create_time asc;

-- name: GetOrgMemberRole :one
select role from org_members where org_id = @org_id and customer_id = @customer_id;

-- name: GetOrgInvitationByToken :one
select * from org_invitations where token = @token;

-- name: GetOrgInvitationsByOrgID :many
select * from org_invitations
where org_id = @org_id
order by create_time desc;
