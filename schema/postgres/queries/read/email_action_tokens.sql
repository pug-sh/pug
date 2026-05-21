-- name: GetValidEmailActionTokenByHashAndPurpose :one
select *
from email_action_tokens
where token_hash = @token_hash
  and purpose = @purpose
  and consumed_at is null
  and expires_at > now();

-- name: GetValidEmailActionTokenByHash :one
-- Purpose-agnostic lookup: token_hash is unique, so the hash alone identifies
-- the token. The caller inspects the returned purpose to decide how to redeem
-- it (e.g. login vs invite), rejecting purposes it does not handle.
select *
from email_action_tokens
where token_hash = @token_hash
  and consumed_at is null
  and expires_at > now();
