-- name: DeleteExpiredRefreshTokens :execrows
-- Used and revoked rows too, but never before expires_at: a used row is what
-- catches a replayed token.
delete from refresh_tokens
where expires_at <= now();

-- name: DeleteDeadMCPGrants :execrows
-- Revoked, never exchanged in time, or out of tokens. A grant whose code still
-- awaits its exchange stays. Run after DeleteExpiredRefreshTokens.
delete from mcp_grants g
where g.revoke_time is not null
  or (g.code_used_at is null and g.code_expires_at <= now())
  or (g.code_used_at is not null
    and not exists (select 1 from refresh_tokens rt where rt.mcp_grant_id = g.id));

-- name: DeleteExpiredEmailActionTokens :execrows
-- Used rows too, but never before expires_at: ResendInvite's cap counts them.
delete from email_action_tokens
where expires_at <= now();
