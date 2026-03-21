-- name: CreateOrgMember :one
insert into org_members (customer_id, org_id, role)
values (@customer_id, @org_id, @role)
returning *;

-- name: GetOrgMemberRole :one
select role from org_members where org_id = @org_id and customer_id = @customer_id;

-- name: DeleteOrgMember :execrows
delete from org_members where org_id = @org_id and customer_id = @customer_id;

-- name: DeleteOrgMemberIfNotLastAdmin :execrows
with target as (
  select role from org_members
  where org_id = @org_id and customer_id = @customer_id
),
admin_count as (
  select count(*) as cnt from org_members
  where org_id = @org_id and role = 'ORG_ROLE_ADMIN'
)
delete from org_members om
where om.org_id = @org_id and om.customer_id = @customer_id
  and (
    (select role from target) != 'ORG_ROLE_ADMIN'
    or (select cnt from admin_count) > 1
  );
