-- +goose Up
create table org_invitations (
  create_time timestamptz not null default now(),
  email varchar(255) not null,
  expires_at timestamptz not null,
  id char(20) primary key,
  inviter_id char(20) references customers(id) on delete set null,
  org_id char(20) not null references orgs(id) on delete cascade,
  -- role stores dashboard.orgs.v1.OrgRole proto enum names (same set as org_members).
  role varchar(30) not null default 'ORG_ROLE_MEMBER',
  status varchar(30) not null default 'INVITATION_STATUS_PENDING',
  token char(32) not null unique,
  constraint org_invitations_status_check check (status in ('INVITATION_STATUS_PENDING', 'INVITATION_STATUS_ACCEPTED')),
  constraint org_invitations_role_check check (role in ('ORG_ROLE_ADMIN', 'ORG_ROLE_MEMBER', 'ORG_ROLE_VIEWER'))
);

create unique index org_invitations_org_email_pending
  on org_invitations (org_id, lower(email))
  where status = 'INVITATION_STATUS_PENDING';

-- +goose Down
drop table org_invitations;
