-- +goose Up
alter table org_invitations
  add column role varchar(30) not null default 'ORG_ROLE_MEMBER',
  add constraint org_invitations_role_check check (role in ('ORG_ROLE_ADMIN', 'ORG_ROLE_MEMBER'));

-- +goose Down
alter table org_invitations
  drop constraint org_invitations_role_check,
  drop column role;
