-- +goose Up
alter table org_members
  add constraint org_members_role_check
  check (role in ('ORG_ROLE_ADMIN', 'ORG_ROLE_MEMBER'));


-- +goose Down
alter table org_members
  drop constraint org_members_role_check;
