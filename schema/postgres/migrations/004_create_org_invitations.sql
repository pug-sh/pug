-- +goose Up
create table org_invitations (
  create_time timestamptz not null default now(),
  email varchar(255) not null,
  expires_at timestamptz not null,
  id char(20) primary key,
  inviter_id char(20) not null references customers(id),
  org_id char(20) not null references orgs(id) on delete cascade,
  status varchar(20) not null default 'pending',
  token char(32) not null unique
);

create unique index org_invitations_org_email_pending
  on org_invitations (org_id, email)
  where status = 'pending';

-- +goose Down
drop table org_invitations;
