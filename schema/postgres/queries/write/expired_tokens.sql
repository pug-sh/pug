-- Delete every expired token, including consumed and revoked rows. The expiry
-- timestamp is the retention boundary; status flags must not shorten it.
-- name: DeleteExpiredRefreshTokens :execrows
delete from refresh_tokens
where expires_at <= now();

-- name: DeleteExpiredEmailActionTokens :execrows
delete from email_action_tokens
where expires_at <= now();
