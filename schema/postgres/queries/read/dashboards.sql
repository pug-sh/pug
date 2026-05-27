-- name: GetDashboardByIDAndProjectID :one
select *
from dashboards
where id = @id and project_id = @project_id;

-- name: ListDashboardsByProjectID :many
select *
from dashboards
where project_id = @project_id
order by create_time asc;

-- name: ListDashboardTilesByProjectID :many
select dt.*
from dashboard_tiles dt
join dashboards d on d.id = dt.dashboard_id
where d.project_id = @project_id
order by dt.create_time asc;

-- name: ListDashboardTilesByDashboardIDAndProjectID :many
select dt.*
from dashboard_tiles dt
join dashboards d on d.id = dt.dashboard_id
where dt.dashboard_id = @dashboard_id and d.project_id = @project_id
order by dt.create_time asc;

-- name: ListDashboardTileIDsByDashboardIDAndProjectID :many
-- ID-only variant used by Upsert to plan the reconcile without paying for the
-- full row payloads. The dashboard join enforces project ownership.
select dt.id
from dashboard_tiles dt
join dashboards d on d.id = dt.dashboard_id
where dt.dashboard_id = @dashboard_id and d.project_id = @project_id;

-- name: LockDashboardByIDAndProjectID :one
-- Acquires a row lock on the dashboard for the duration of the calling tx —
-- used by Upsert to serialize concurrent edits to the same dashboard so the
-- documented last-write-wins contract holds (two interleaved transactions
-- without the lock can both insert and commit their own tile, producing a
-- merge rather than one tile from one of the inputs).
select *
from dashboards
where id = @id and project_id = @project_id
for update;

-- name: ListDashboardTilesByDashboardID :many
select *
from dashboard_tiles
where dashboard_id = @dashboard_id
order by create_time asc;

-- name: GetDashboardShareByDashboardID :one
select *
from dashboard_shares
where dashboard_id = @dashboard_id;

-- name: GetEnabledDashboardShareByID :one
select *
from dashboard_shares
where id = @id and enabled = true;
