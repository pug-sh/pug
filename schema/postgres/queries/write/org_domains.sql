-- name: CreateOrgDomain :one
insert into org_domains (id, org_id, domain, verification_token)
values (@id, @org_id, @domain, @verification_token)
returning *;

-- name: MarkOrgDomainVerifiedByDNS :one
update org_domains
set verified_at = now(), verification_method = 'dns'
where id = @id and org_id = @org_id and verified_at is null
returning *;

-- name: UpsertOrgDomainVerifiedByOperator :one
insert into org_domains (id, org_id, domain, verification_token, verified_at, verification_method)
values (@id, @org_id, @domain, @verification_token, now(), 'operator')
on conflict (org_id, domain) do update
set verified_at = coalesce(org_domains.verified_at, now()),
    verification_method = 'operator'
returning *;

-- name: DeleteOrgDomain :execrows
delete from org_domains where id = @id and org_id = @org_id;

-- name: DeleteOrgDomainByOrgIDAndDomain :one
delete from org_domains where org_id = @org_id and domain = @domain
returning sso_connection_id;

-- name: UpdateOrgDomainRequireSSO :one
-- Turning it on needs a verified claim that an SSO sign-in has proven.
update org_domains
set require_sso = @require_sso
where id = @id and org_id = @org_id
  and (not @require_sso::boolean or (verified_at is not null and sso_seen_at is not null))
returning *;

-- name: UnenforceOrgDomainRequireSSO :execrows
update org_domains set require_sso = false where domain = @domain and require_sso;

-- name: MarkOrgDomainsSSOSeen :exec
update org_domains
set sso_seen_at = now()
where domain = @domain and verified_at is not null and sso_seen_at is null;

-- name: ClearOrgDomainsSSOSeen :exec
update org_domains set sso_seen_at = null where domain = any(@domains::text[]);

-- name: AutoJoinOrgsByDomain :many
insert into org_members (org_id, customer_id, role, joined_via_domain)
select d.org_id, @customer_id::char(20), o.auto_join_role, d.domain
from org_domains d
join orgs o on o.id = d.org_id
where d.domain = @domain
  and d.verified_at is not null
  and o.auto_join_role is not null
  and not exists (
    select 1 from org_invitations i
    where i.org_id = d.org_id and lower(i.email) = lower(@email)
      and i.status = 'INVITATION_STATUS_PENDING' and i.expires_at > now()
  )
on conflict (org_id, customer_id) do nothing
returning org_id;

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
