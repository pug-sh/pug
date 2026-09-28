-- +goose Up
create table instance_audit (
  id char(20) primary key,
  create_time timestamptz not null default now(),
  actor_id char(20) not null,
  action varchar(80) not null,
  target_type varchar(40) not null,
  target_id char(20) not null,
  reason text not null default '',
  details jsonb not null default '{}'::jsonb
);
create index instance_audit_target_idx on instance_audit (target_type, target_id, create_time desc);

-- +goose Down
drop table instance_audit;
