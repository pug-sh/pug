-- name: GetLiveSessionProvenDomain :one
-- A dashboard session is live while its family's newest token is: unused,
-- unrevoked and unexpired. Sign-out revokes the whole family.
select proven_domain
from refresh_tokens
where family_id = @family_id
  and customer_id = @customer_id
  and mcp_grant_id is null
  and consumed_at is null
  and revoked_at is null
  and expires_at > now();
