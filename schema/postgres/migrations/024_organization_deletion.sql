-- +goose Up
alter table orgs add column deletion_state text not null default 'active'
  check (deletion_state in ('active', 'pending_deletion', 'deleting', 'failed'));

alter table deletion_operations drop constraint deletion_operations_target_type_check;
alter table deletion_operations add constraint deletion_operations_target_type_check
  check (target_type in ('project', 'organization'));
alter table deletion_operations drop constraint deletion_operations_status_check;
alter table deletion_operations add constraint deletion_operations_status_check
  check (status in ('pending_deletion', 'deleting', 'failed', 'deleted', 'cancelled'));

-- A provider event may reactivate billing during the 24-hour window. That is
-- allowed while pending so the purger can stop before touching live data. Once
-- purging starts, a live subscription must not race in after the final check.
-- FOR SHARE serializes this check with the purger's org-row state transition.
-- +goose StatementBegin
create function prevent_live_billing_during_org_purge() returns trigger language plpgsql as $$
declare org_state text;
begin
  if new.status in ('active', 'past_due') then
    select deletion_state into org_state from orgs where id=new.org_id for share;
    if org_state in ('deleting', 'failed') then
      raise exception 'cannot activate billing for an organization being deleted' using errcode = '23514';
    end if;
  end if;
  return new;
end;
$$;
-- +goose StatementEnd
create trigger prevent_live_billing_during_org_purge
  before insert or update of status, org_id on billing_subscriptions
  for each row execute function prevent_live_billing_during_org_purge();

-- +goose Down
drop trigger prevent_live_billing_during_org_purge on billing_subscriptions;
drop function prevent_live_billing_during_org_purge();
-- A started purge may already have removed project data and cannot be made
-- active again by a schema rollback.
-- +goose StatementBegin
do $$
begin
  if exists (
    select 1 from deletion_operations
    where target_type = 'organization'
      and started_at is not null
      and status in ('pending_deletion', 'deleting', 'failed')
  ) then
    raise exception 'cannot roll back organization deletion while an irreversible purge is incomplete';
  end if;
end;
$$;
-- +goose StatementEnd
update projects set deletion_state = 'active'
  where deletion_state in ('pending_deletion', 'failed')
    and id in (
      select s.project_id
      from deletion_project_steps s
      join deletion_operations o on o.id = s.operation_id
      where o.target_type = 'organization' and o.started_at is null
    );
delete from deletion_project_steps
  where operation_id in (select id from deletion_operations where target_type = 'organization');
delete from deletion_operations where target_type = 'organization';
alter table deletion_operations drop constraint deletion_operations_status_check;
alter table deletion_operations add constraint deletion_operations_status_check
  check (status in ('pending_deletion', 'deleting', 'failed', 'deleted'));
alter table deletion_operations drop constraint deletion_operations_target_type_check;
alter table deletion_operations add constraint deletion_operations_target_type_check
  check (target_type = 'project');
alter table orgs drop column deletion_state;
