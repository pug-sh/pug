-- +goose Up
-- A grant reads every project in the orgs its person was in at approval, so it
-- names none. Those made before this were approved for one project: revoke them, so
-- their people consent again. The column goes once no running server uses it.
alter table mcp_grants alter column project_id drop not null;
update mcp_grants set revoke_time = now() where project_id is not null and revoke_time is null;

-- +goose Down
-- A grant that names no project can't be kept where every grant must name one.
delete from mcp_grants where project_id is null;
alter table mcp_grants alter column project_id set not null;
