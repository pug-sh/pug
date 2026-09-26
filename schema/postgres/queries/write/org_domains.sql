-- name: GetOrgDomainByID :one
select * from org_domains where id = @id and org_id = @org_id;

-- name: GetOrgDomainByOrgIDAndDomain :one
select * from org_domains where org_id = @org_id and domain = @domain;

-- name: CountOrgDomainsByOrgID :one
select count(*) from org_domains where org_id = @org_id;

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

-- name: DeleteOrgDomainByOrgIDAndDomain :execrows
delete from org_domains where org_id = @org_id and domain = @domain;

-- name: MarkOrgDomainsSSOSeen :exec
update org_domains
set sso_seen_at = now()
where domain = @domain and verified_at is not null and sso_seen_at is null;

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

-- name: IsOrgCreationRestricted :one
select (
  exists (
    select 1 from org_domains d
    join orgs o on o.id = d.org_id
    where d.domain = @domain and d.verified_at is not null
      and not o.members_can_create_orgs
  )
  and not exists (
    select 1 from org_domains d
    join org_members m on m.org_id = d.org_id
    where d.domain = @domain and d.verified_at is not null
      and m.customer_id = @customer_id::char(20) and m.role = 'ORG_ROLE_ADMIN'
  )
)::boolean as restricted;
