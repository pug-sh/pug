-- name: CreateOrgMember :one
insert into org_members (customer_id, org_id, role)
values (@customer_id, @org_id, @role)
returning *;

-- name: DeleteOrgMember :exec
delete from org_members where org_id = @org_id and customer_id = @customer_id;
