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
