-- name: ListOrgDomainsByOrgID :many
-- Only a verified claim may learn other orgs' settings for its domain.
select
  d.id,
  d.org_id,
  d.domain,
  d.verification_token,
  d.verified_at,
  d.verification_method,
  d.require_sso,
  d.sso_seen_at,
  d.create_time,
  d.update_time,
  (d.verified_at is not null and exists (
    select 1 from org_domains d2
    join orgs o2 on o2.id = d2.org_id
    where d2.domain = d.domain
      and d2.org_id <> d.org_id
      and d2.verified_at is not null
      and not o2.members_can_create_orgs
  ))::boolean as org_creation_restricted_elsewhere,
  (d.verified_at is not null and exists (
    select 1 from org_domains d2
    where d2.domain = d.domain
      and d2.org_id <> d.org_id
      and d2.verified_at is not null
      and d2.require_sso
  ))::boolean as sso_required_elsewhere
from org_domains d
where d.org_id = @org_id
order by d.create_time asc, d.id asc;

-- name: ListOrgDomainsByDomain :many
select
  d.id,
  d.org_id,
  d.domain,
  d.verified_at,
  d.verification_method,
  d.require_sso,
  d.sso_seen_at,
  d.create_time,
  o.display_name as org_display_name,
  o.auto_join_role,
  o.members_can_create_orgs
from org_domains d
join orgs o on o.id = d.org_id
where d.domain = @domain
order by d.verified_at asc nulls last, d.create_time asc, d.id asc;
