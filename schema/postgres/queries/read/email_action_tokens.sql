-- name: GetValidEmailActionTokenByHashAndPurpose :one
select *
from email_action_tokens
where token_hash = @token_hash
  and purpose = @purpose
  and consumed_at is null
  and expires_at > now();

-- name: CountRecentEmailActionTokensByInvitation :one
-- One row per email sent for the invitation, counted over a trailing window so
-- the cap on ResendInvite (which mails an address that need not belong to any
-- pug user) bounds a burst without bricking the invitation forever.
select count(*)
from email_action_tokens
where org_invitation_id = @org_invitation_id
  and purpose = @purpose
  and create_time > @since;
