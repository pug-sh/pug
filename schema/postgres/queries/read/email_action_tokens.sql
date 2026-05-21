-- name: GetValidEmailActionTokenByHashAndPurpose :one
select *
from email_action_tokens
where token_hash = @token_hash
  and purpose = @purpose
  and consumed_at is null
  and expires_at > now();
