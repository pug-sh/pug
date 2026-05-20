-- +goose Up
alter table org_invitations
  add constraint org_invitations_status_check
  check (status in ('INVITATION_STATUS_PENDING', 'INVITATION_STATUS_ACCEPTED'));


-- +goose Down
alter table org_invitations
  drop constraint org_invitations_status_check;
