-- name: CreateOrgMember :one
insert into org_members (customer_id, org_id, role)
values (@customer_id, @org_id, @role)
returning *;

-- name: GetOrgMemberRole :one
select role from org_members where org_id = @org_id and customer_id = @customer_id;

-- name: DeleteOrgMember :execrows
delete from org_members where org_id = @org_id and customer_id = @customer_id;

-- name: DeleteOrgMemberIfNotLastAdmin :execrows
-- 'locked' acquires row-level locks on every member of the org so concurrent
-- callers serialize on the admin set — without it, two snapshots could both
-- see admin_count = 2 and both succeed, leaving the org with zero admins.
with locked as (
  select customer_id, role from org_members
  where org_id = @org_id
  for update
),
target as (
  select role from locked where customer_id = @customer_id
),
admin_count as (
  select count(*) as cnt from locked where role = 'ORG_ROLE_ADMIN'
)
delete from org_members om
where om.org_id = @org_id and om.customer_id = @customer_id
  and (
    (select role from target) != 'ORG_ROLE_ADMIN'
    or (select cnt from admin_count) > 1
  );

-- name: DeleteOrgMemberIfNotLastAdminAndNotLastMember :execrows
-- See DeleteOrgMemberIfNotLastAdmin for the rationale behind the 'locked' CTE.
with locked as (
  select customer_id, role from org_members
  where org_id = @org_id
  for update
),
target as (
  select role from locked where customer_id = @customer_id
),
admin_count as (
  select count(*) as cnt from locked where role = 'ORG_ROLE_ADMIN'
),
member_count as (
  select count(*) as cnt from locked
)
delete from org_members om
where om.org_id = @org_id and om.customer_id = @customer_id
  and (select cnt from member_count) > 1
  and (
    (select role from target) != 'ORG_ROLE_ADMIN'
    or (select cnt from admin_count) > 1
  );

-- name: UpdateOrgMemberRole :one
update org_members
set role = @role
where org_id = @org_id and customer_id = @customer_id
returning *;
