-- +goose Up
-- Project deletion, phase 1 (docs/architecture/project-deletion.md).

-- Null is live.
alter table projects add column deletion_time timestamptz;

-- Same name, so the name check in internal/core/projects keeps matching it.
alter table projects drop constraint projects_org_id_display_name_key;
create unique index projects_org_id_display_name_key
  on projects (org_id, display_name) where deletion_time is null;

-- Usage is the org's billing record, so it outlives the project.
alter table usage_daily drop constraint usage_daily_project_id_fkey;

-- No foreign keys, so the row outlives the project.
create table project_deletions (
  display_name varchar(150) not null default '',
  done_at timestamptz,
  error text not null default '',
  -- Null only for a project deleted before this shipped.
  org_id char(20),
  project_id char(20) primary key,
  requested_at timestamptz not null default now(),
  requested_by text not null,
  round_started_at timestamptz,
  rounds integer not null default 0,
  status text not null,
  update_time timestamptz not null default now()
);

create trigger update_timestamp before
update on project_deletions for each row execute procedure moddatetime(update_time);

-- +goose Down
-- The full unique constraint and the foreign key cannot come back over hidden
-- projects, or over usage rows whose project is gone.
drop table if exists project_deletions;

delete from projects where deletion_time is not null;
delete from usage_daily d
where not exists (select 1 from projects p where p.id = d.project_id);

alter table usage_daily
  add constraint usage_daily_project_id_fkey
    foreign key (project_id) references projects(id) on delete cascade;

drop index if exists projects_org_id_display_name_key;
alter table projects
  add constraint projects_org_id_display_name_key unique (org_id, display_name);
alter table projects drop column if exists deletion_time;
