-- name: GetDashboardByIDAndProjectID :one
select *
from dashboards
where id = @id and project_id = @project_id;

-- name: ListDashboardsByProjectID :many
select *
from dashboards
where project_id = @project_id
order by create_time asc;

-- name: ListDashboardInsightsByProjectID :many
select di.*
from dashboard_insights di
join dashboards d on d.id = di.dashboard_id
where d.project_id = @project_id
order by di.create_time asc;

-- name: ListDashboardInsightsByDashboardIDAndProjectID :many
select di.*
from dashboard_insights di
join dashboards d on d.id = di.dashboard_id
where di.dashboard_id = @dashboard_id and d.project_id = @project_id
order by di.create_time asc;
