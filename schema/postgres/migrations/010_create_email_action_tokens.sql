-- +goose Up
alter table customers
  add column email_verified_at timestamptz null;

create table email_action_tokens (
  id char(20) primary key,
  customer_id char(20) null references customers(id) on delete cascade,
  email varchar(255) not null,
  purpose varchar(30) not null,
  token_hash varchar(64) not null unique,
  org_invitation_id char(20) null references org_invitations(id) on delete cascade,
  expires_at timestamptz not null,
  consumed_at timestamptz null,
  create_time timestamptz not null default now()
);

create index email_action_tokens_email_purpose_idx
  on email_action_tokens (lower(email), purpose)
  where consumed_at is null;

create index email_action_tokens_customer_purpose_idx
  on email_action_tokens (customer_id, purpose)
  where consumed_at is null and customer_id is not null;

create index email_action_tokens_org_invitation_idx
  on email_action_tokens (org_invitation_id)
  where org_invitation_id is not null;

-- +goose Down
drop table email_action_tokens;

alter table customers
  drop column email_verified_at;
