-- name: CreateRefreshToken :one
insert into refresh_tokens (
  id,
  customer_id,
  family_id,
  token_hash,
  expires_at
)
values (
  @id,
  @customer_id,
  @family_id,
  @token_hash,
  @expires_at
)
returning *;

-- name: GetRefreshTokenByHashForUpdate :one
-- Locks the row FOR UPDATE so concurrent refreshes of the same token serialize:
-- the first to commit consumes it, and the rest re-read consumed_at set and fall
-- into the reuse-detection path. Unlike the email-action lookup, this does NOT
-- filter on consumed_at/revoked_at — the caller MUST see an already-consumed or
-- revoked row to detect token replay and decide invalidity in Go.
select *
from refresh_tokens
where token_hash = @token_hash
for update;

-- name: ConsumeRefreshToken :one
-- Marks the current token rotated. Gated on consumed_at is null so a lost race
-- returns zero rows rather than double-consuming.
update refresh_tokens
set consumed_at = now()
where id = @id
  and consumed_at is null
  and revoked_at is null
returning *;

-- name: RevokeRefreshTokenFamily :execrows
-- Reuse-detection / defensive kill: revoke every live token in a rotation chain.
update refresh_tokens
set revoked_at = now()
where family_id = @family_id
  and revoked_at is null;

-- name: RevokeRefreshTokenFamilyByHash :execrows
-- Sign-out: revoke the whole rotation family that the presented token belongs to,
-- given any (even already-consumed) token in it. A non-matching hash revokes
-- nothing (zero rows) so logout with a stale token is a clean no-op.
update refresh_tokens
set revoked_at = now()
where family_id = (
    select rt.family_id from refresh_tokens rt where rt.token_hash = @token_hash
  )
  and revoked_at is null;
