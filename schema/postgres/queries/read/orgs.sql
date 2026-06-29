-- name: GetOrgByID :one
select * from orgs where id = @id;

-- name: GetOrgsByCustomerID :many
select o.*
from orgs o
join org_members om on om.org_id = o.id
where om.customer_id = @customer_id
order by o.create_time asc, o.id asc;

-- name: GetOrgsWithRoleByCustomerID :many
select o.id, o.display_name, o.create_time, o.update_time, m.role
from orgs o
join org_members m on m.org_id = o.id
where m.customer_id = @customer_id
order by o.create_time asc, o.id asc;

-- name: GetOrgWithRoleByIDAndCustomerID :one
select o.id, o.display_name, o.create_time, o.update_time, m.role
from orgs o
join org_members m on m.org_id = o.id
where o.id = @org_id and m.customer_id = @customer_id;
