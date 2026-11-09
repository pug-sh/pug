-- name: CreateMCPGrant :exec
insert into mcp_grants (
  client_id,
  client_name,
  code_challenge,
  code_expires_at,
  code_hash,
  customer_id,
  id,
  proven_domain,
  redirect_uri
)
values (
  @client_id,
  @client_name,
  @code_challenge,
  @code_expires_at,
  @code_hash,
  @customer_id,
  @id,
  @proven_domain,
  @redirect_uri
);

-- name: GetMCPGrantByCodeHashForUpdate :one
-- Unfiltered, so a used code is found and its replay revokes the grant.
select client_id, code_challenge, code_expires_at, code_used_at, customer_id, id, redirect_uri, revoke_time
from mcp_grants
where code_hash = @code_hash
for update;

-- name: UseMCPGrantCode :exec
update mcp_grants
set code_used_at = now(),
  last_used_time = now()
where id = @id;

-- name: MarkMCPGrantUsed :exec
update mcp_grants
set last_used_time = now()
where id = @id;

-- name: RevokeMCPGrant :exec
update mcp_grants
set revoke_time = now()
where id = @id
  and revoke_time is null;

-- name: RevokeCustomerMCPGrant :execrows
-- Scoped by customer_id, so a grant id never revokes another person's grant.
update mcp_grants
set revoke_time = now()
where id = @id
  and customer_id = @customer_id
  and revoke_time is null;
