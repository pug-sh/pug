-- name: GetMCPAccessByTokenHash :one
-- The token is unexpired, its grant unrevoked, its project live, and its person in
-- the project's org since before the grant, so being added back doesn't revive it.
select g.customer_id, g.id as grant_id, g.project_id
from refresh_tokens rt
join mcp_grants g on g.id = rt.mcp_grant_id
join projects p on p.id = g.project_id
join org_members om on om.org_id = p.org_id and om.customer_id = g.customer_id
where rt.access_token_hash = @access_token_hash
  and rt.access_expires_at > now()
  and g.revoke_time is null
  and p.deletion_time is null
  and om.create_time <= g.create_time;

-- name: GetMCPGrantMemberEmail :one
-- No row once the person has left the project's org, even if added back since, or
-- the project is deleted.
select c.email
from mcp_grants g
join customers c on c.id = g.customer_id
join projects p on p.id = g.project_id
join org_members om on om.org_id = p.org_id and om.customer_id = g.customer_id
where g.id = @id
  and p.deletion_time is null
  and om.create_time <= g.create_time;
