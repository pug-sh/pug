-- +goose Up
alter table projects add column deletion_state text not null default 'active'
  check (deletion_state in ('active', 'pending_deletion', 'deleting', 'failed'));

-- The compliance ledger must survive a project row's eventual removal.
alter table compliance_requests drop constraint compliance_requests_project_id_fkey;
create index compliance_requests_project_id_idx on compliance_requests(project_id);

-- The operation and its project step survive removal of the live project row.
create table deletion_operations (
  id char(20) primary key,
  target_type text not null check (target_type = 'project'),
  target_id char(20) not null,
  target_name varchar(150) not null,
  org_id char(20) not null,
  actor_id char(20) not null,
  reason text not null default '',
  status text not null check (status in ('pending_deletion', 'deleting', 'failed', 'deleted')),
  requested_at timestamptz not null default now(),
  purge_after timestamptz not null,
  started_at timestamptz,
  finished_at timestamptz,
  last_error text not null default ''
);
create unique index deletion_operations_live_target_idx
  on deletion_operations(target_type, target_id)
  where status in ('pending_deletion', 'deleting', 'failed');
create index deletion_operations_org_idx on deletion_operations(org_id, requested_at desc);

create table deletion_project_steps (
  operation_id char(20) not null references deletion_operations(id),
  project_id char(20) not null,
  project_name varchar(150) not null,
  clickhouse_done_at timestamptz,
  postgres_done_at timestamptz,
  primary key (operation_id, project_id)
);

-- +goose Down
drop table deletion_project_steps;
drop table deletion_operations;
alter table compliance_requests
  add constraint compliance_requests_project_id_fkey foreign key (project_id) references projects(id) on delete cascade not valid;
drop index compliance_requests_project_id_idx;
alter table projects drop column deletion_state;
