-- name: CreateProject :one
-- The project row only. Its starter public key is a second statement
-- (CreateApiKey), which projects.CreateProjectInTx runs in the same transaction
-- so a project can never commit without one. A project never gets a private key
-- implicitly; those are created explicitly via CreateApiKey.
insert into projects (display_name, id, org_id, reporting_timezone)
values (@display_name, @id, @org_id, @reporting_timezone)
returning *;

-- name: CreateProjectAsAdmin :one
-- CreateProject plus the admin guard. Returns no row for a non-admin, which
-- leaves the caller's transaction without a project — so the starter key insert
-- that follows never runs either.
with check_admin as (
  select 1 from org_members
  where org_id = @org_id and customer_id = @customer_id and role = 'ORG_ROLE_ADMIN'
)
insert into projects (display_name, id, org_id, reporting_timezone)
select @display_name, @id, @org_id, @reporting_timezone
where exists (select 1 from check_admin)
returning *;

-- name: GetProjectByIDForUpdate :one
-- The delete request's lock. FOR UPDATE, unlike the hide's own row lock, also
-- holds off inserts that reference the project until the request commits.
select * from projects
where org_id = @org_id and id = @id and deletion_time is null
for update;

-- name: GetAnyProjectByIDForUpdate :one
-- The operator's lock: by id alone, hidden projects included.
select * from projects
where id = @id
for update;

-- name: CreateProjectDeletion :exec
insert into project_deletions (display_name, org_id, project_id, requested_by, status)
values (@display_name, @org_id, @project_id, @requested_by, 'pending');

-- name: CreateOrphanProjectDeletion :execrows
-- For an id with no projects row. No org, so the job skips its usage count.
insert into project_deletions (project_id, requested_by, status)
values (@project_id, @requested_by, 'pending')
on conflict (project_id) do nothing;

-- name: DeleteApiKeysByProjectID :many
delete from api_keys where project_id = @project_id returning token;

-- name: DeleteDashboardSharesByProjectID :exec
delete from dashboard_shares where project_id = @project_id;

-- name: DeleteCampaignsByProjectID :exec
delete from campaigns where project_id = @project_id;

-- name: HideProject :exec
update projects
set deletion_time = now(), fcm_service_json = null
where id = @id;

-- name: UpdateFCMServiceJSON :one
update projects
set fcm_service_json = @fcm_service_json
where org_id = @org_id and id = @id and deletion_time is null
returning *;

-- name: UpdateProjectMeta :one
-- Partial update: a NULL param leaves the column unchanged, so callers can update
-- display_name and reporting_timezone independently. A present empty string is
-- written (an empty reporting_timezone resets to UTC), unlike an omitted NULL.
update projects
set display_name       = coalesce(sqlc.narg('display_name'), display_name),
    reporting_timezone = coalesce(sqlc.narg('reporting_timezone'), reporting_timezone)
where org_id = @org_id and id = @id and deletion_time is null
returning *;
