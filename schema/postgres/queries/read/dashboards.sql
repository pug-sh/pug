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
