-- name: GetSSOConnectionByID :one
select * from org_sso_connections where id = @id;

-- name: GetSSOConnectionByIDForUpdate :one
select * from org_sso_connections where id = @id and org_id = @org_id for update;

-- name: GetSSOConnectionIssuerForShare :one
select issuer_url from org_sso_connections where id = @id for share;

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

-- name: CreateSSOConnection :one
insert into org_sso_connections (id, org_id, label, issuer_url, client_id, client_secret_ciphertext)
values (@id, @org_id, @label, @issuer_url, @client_id, @client_secret_ciphertext)
returning *;

-- name: UpdateSSOConnection :one
update org_sso_connections
set label = @label,
    issuer_url = @issuer_url,
    client_id = @client_id,
    client_secret_ciphertext = @client_secret_ciphertext
where id = @id and org_id = @org_id
returning *;

-- name: DeleteSSOConnection :execrows
delete from org_sso_connections where id = @id and org_id = @org_id;

-- name: ListSSOConnectionDomains :many
select domain from org_domains
where sso_connection_id = @sso_connection_id::char(20) and verified_at is not null
order by domain;

-- name: ListOrgDomainsWithSSOConnection :many
select id, domain, sso_connection_id from org_domains
where org_id = @org_id and sso_connection_id is not null
order by domain;

-- name: ListOrgDomainsByIDs :many
select * from org_domains where org_id = @org_id and id = any(@ids::text[]);

-- name: DetachOrgDomainsFromSSOConnection :many
update org_domains
set sso_connection_id = null
where org_id = @org_id and sso_connection_id = @sso_connection_id::char(20)
  and not (id = any(@keep_ids::text[]))
returning domain;

-- name: AttachOrgDomainsToSSOConnection :execrows
-- Skips a domain another connection of the org already signs in.
update org_domains
set sso_connection_id = @sso_connection_id::char(20)
where org_id = @org_id and id = any(@ids::text[]) and verified_at is not null
  and (sso_connection_id is null or sso_connection_id = @sso_connection_id::char(20));
