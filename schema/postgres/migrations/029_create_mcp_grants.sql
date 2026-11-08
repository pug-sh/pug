-- +goose Up
-- OAuth for /mcp: grants, and their token pairs in refresh_tokens.

-- One approval: one person, one project, one client.
create table mcp_grants (
  client_id text not null,
  client_name text not null,
  code_challenge text not null,
  code_expires_at timestamptz not null,
  -- Kept after use, to catch a replay.
  code_hash varchar(64) not null unique,
  code_used_at timestamptz,
  create_time timestamptz not null default now(),
  customer_id char(20) not null references customers(id) on delete cascade,
  id char(20) primary key,
  last_used_time timestamptz,
  project_id char(20) not null references projects(id) on delete cascade,
  proven_domain varchar(253),
  redirect_uri text not null,
  revoke_time timestamptz
);

create index mcp_grants_customer_id_idx on mcp_grants (customer_id);
create index mcp_grants_project_id_idx on mcp_grants (project_id);

-- An MCP token pair sets all three. RefreshSession and sign-out skip these rows.
alter table refresh_tokens
  add column access_expires_at timestamptz,
  add column access_token_hash varchar(64) unique,
  add column mcp_grant_id char(20) references mcp_grants(id) on delete cascade,
  add constraint refresh_tokens_mcp_check
    check ((mcp_grant_id is null) = (access_token_hash is null)
      and (mcp_grant_id is null) = (access_expires_at is null));

create index refresh_tokens_mcp_grant_id_idx
  on refresh_tokens (mcp_grant_id) where mcp_grant_id is not null;

-- +goose Down
-- Without the columns these rows would pass for dashboard sessions. The lock stops
-- a pair minted during the Down from landing after the delete.
lock table refresh_tokens in exclusive mode;
delete from refresh_tokens where mcp_grant_id is not null;
drop index if exists refresh_tokens_mcp_grant_id_idx;
alter table refresh_tokens
  drop constraint if exists refresh_tokens_mcp_check,
  drop column if exists mcp_grant_id,
  drop column if exists access_token_hash,
  drop column if exists access_expires_at;
drop table if exists mcp_grants;
