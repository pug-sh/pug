-- +goose Up
-- Durable, rotating refresh-token store. The access JWT is short-lived (1h); a
-- refresh token is the long-lived (sliding 90-day) secret a client exchanges for
-- a fresh access+refresh pair. Stored as a sha256 hash (never plaintext), exactly
-- like email_action_tokens.
--
-- family_id groups one rotation chain: every issue() starts a new family, and each
-- refresh consumes the current token and inserts its successor in the same family.
-- If a token that has ALREADY been consumed is presented again (replay of a stolen
-- or leaked token), the whole family is revoked — a standard refresh-token
-- reuse-detection scheme.
create table refresh_tokens (
  id char(20) primary key,
  customer_id char(20) not null references customers(id) on delete cascade,
  family_id char(20) not null,
  token_hash varchar(64) not null unique,
  expires_at timestamptz not null,
  consumed_at timestamptz null, -- set when rotated (exchanged for a successor)
  revoked_at timestamptz null,  -- set on sign-out or reuse-detection family kill
  create_time timestamptz not null default now()
);

-- Reuse-detection revokes by family; this supports that UPDATE.
create index refresh_tokens_family_idx
  on refresh_tokens (family_id);

-- Supports a future cleanup job that prunes expired live tokens without scanning
-- the whole table (mirrors email_action_tokens_expires_at_idx).
create index refresh_tokens_expires_at_idx
  on refresh_tokens (expires_at)
  where consumed_at is null and revoked_at is null;

-- +goose Down
drop table refresh_tokens;
