-- +goose Up
-- The retention length each org's deletes use, and a shorter one waiting to
-- replace it (docs/architecture/data-retention.md). No row keeps everything.
create table retention_state (
  -- Null keeps everything.
  days bigint,
  expired_by text,
  org_id char(20) primary key references orgs(id) on delete cascade,
  pending_days bigint,
  -- Null beside pending_days waits for `pug retention expire`.
  pending_since timestamptz,
  update_time timestamptz not null default now()
);

create trigger update_timestamp before
update on retention_state for each row execute procedure moddatetime(update_time);

-- +goose Down
drop table if exists retention_state;
