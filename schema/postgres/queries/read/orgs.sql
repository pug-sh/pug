-- name: GetOrgByID :one
select * from orgs where id = @id;

-- name: GetOrgsByCustomerID :many
select o.*
from orgs o
join org_members om on om.org_id = o.id
where om.customer_id = @customer_id
order by o.create_time asc;
