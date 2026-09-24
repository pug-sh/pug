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
select m.role from org_members m join orgs o on o.id=m.org_id where m.org_id = @org_id and m.customer_id = @customer_id and o.deletion_state='active';

-- name: GetOrgMemberByOrgIDAndCustomerID :one
select
  om.customer_id,
  om.create_time,
  om.org_id,
  om.role,
  c.display_name,
  c.email
from org_members om
join customers c on c.id = om.customer_id
where om.org_id = @org_id and om.customer_id = @customer_id;
