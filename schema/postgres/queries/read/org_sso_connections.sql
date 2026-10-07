-- name: GetSSOConnectionByID :one
select * from org_sso_connections where id = @id;

-- name: GetSSOConnectionByDomain :one
select c.*
from org_domains d
join org_sso_connections c on c.org_id = d.org_id and c.id = d.sso_connection_id
where d.domain = @domain;

-- name: ListSSOConnectionsByOrgID :many
select * from org_sso_connections where org_id = @org_id order by create_time asc, id asc;

-- name: HasSSOConnections :one
select exists (select 1 from org_sso_connections)::boolean;

-- name: CountSSOConnectionsByOrgID :one
select count(*) from org_sso_connections where org_id = @org_id;
