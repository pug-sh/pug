-- name: GetValidEmailActionTokenByHashForUpdate :one
-- Purpose-agnostic lookup that locks the row FOR UPDATE so concurrent
-- redemptions of the same token serialize: the first to commit consumes it, and
-- the rest re-read zero rows (consumed) and fail cleanly with ErrInvalidToken
-- instead of racing on a downstream unique violation (e.g. duplicate customer).
-- token_hash is unique, so the hash alone identifies the token; the caller
-- inspects the returned purpose to decide how to redeem it.
select *
from email_action_tokens
where token_hash = @token_hash
  and consumed_at is null
  and expires_at > now()
for update;

-- name: CreateEmailActionToken :one
insert into email_action_tokens (
  id,
  customer_id,
  email,
  purpose,
  token_hash,
  org_invitation_id,
  expires_at
)
values (
  @id,
  @customer_id,
  @email,
  @purpose,
  @token_hash,
  @org_invitation_id,
  @expires_at
)
returning *;

-- name: ConsumeEmailActionToken :one
update email_action_tokens
set consumed_at = now()
where id = @id
  and consumed_at is null
  and expires_at > now()
returning *;

-- name: InvalidateActiveEmailActionTokensByEmail :execrows
update email_action_tokens
set consumed_at = now()
where lower(email) = lower(@email)
  and purpose = @purpose
  and consumed_at is null
  and expires_at > now();

-- name: InvalidateActiveEmailActionTokensByCustomer :execrows
update email_action_tokens
set consumed_at = now()
where customer_id = @customer_id
  and purpose = @purpose
  and consumed_at is null
  and expires_at > now();

-- name: InvalidateActiveEmailActionTokensByInvitation :execrows
update email_action_tokens
set consumed_at = now()
where org_invitation_id = @org_invitation_id
  and purpose = @purpose
  and consumed_at is null
  and expires_at > now();
