-- name: GetMCPAccessByTokenHash :one
-- The token is unexpired, its grant unrevoked, and its person still in an org they
-- joined before the grant, so being added back doesn't revive it.
select g.customer_id, g.id as grant_id
from refresh_tokens rt
join mcp_grants g on g.id = rt.mcp_grant_id
where rt.access_token_hash = @access_token_hash
  and rt.access_expires_at > now()
  and g.revoke_time is null
  and exists (
    select 1 from org_members om
    where om.customer_id = g.customer_id and om.create_time <= g.create_time
  );

-- name: GetMCPGrantProject :one
-- A live project in an org the grant's person joined before the grant.
select p.*
from mcp_grants g
join org_members om on om.customer_id = g.customer_id and om.create_time <= g.create_time
join projects p on p.org_id = om.org_id
where g.id = @grant_id
  and p.id = @project_id
  and p.deletion_time is null;

-- name: ListMCPGrantProjects :many
-- Every live project in the orgs the grant's person joined before the grant.
select
  o.display_name as org_display_name,
  p.display_name,
  p.id,
  p.org_id
from mcp_grants g
join org_members om on om.customer_id = g.customer_id and om.create_time <= g.create_time
join orgs o on o.id = om.org_id
join projects p on p.org_id = o.id
where g.id = @grant_id
  and p.deletion_time is null
order by o.create_time asc, o.id asc, p.create_time asc, p.id asc;

-- name: GetMCPGrantMemberEmail :one
-- No row once the person has left every org they joined before the grant, even if
-- added back since.
select c.email
from mcp_grants g
join customers c on c.id = g.customer_id
where g.id = @id
  and exists (
    select 1 from org_members om
    where om.customer_id = g.customer_id and om.create_time <= g.create_time
  );

-- name: ListLiveMCPGrantsByCustomerID :many
-- One row per grant and org it reads: unrevoked, with an unexpired refresh token,
-- which only an exchanged code mints, and an org the person joined before the grant.
select
  g.client_id,
  g.client_name,
  g.create_time,
  g.id,
  g.last_used_time,
  o.display_name as org_display_name,
  o.id as org_id
from mcp_grants g
join org_members om on om.customer_id = g.customer_id and om.create_time <= g.create_time
join orgs o on o.id = om.org_id
where g.customer_id = @customer_id
  and g.revoke_time is null
  and exists (
    select 1 from refresh_tokens rt
    where rt.mcp_grant_id = g.id and rt.expires_at > now()
  )
order by g.create_time desc, g.id desc, o.create_time asc, o.id asc;
