-- name: CreateDashboard :one
insert into dashboards (description, display_name, id, project_id, default_time_range, default_granularity)
values (@description, @display_name, @id, @project_id, @default_time_range, @default_granularity)
returning *;

-- name: UpdateDashboard :one
update dashboards
set display_name        = @display_name,
    description         = coalesce(nullif(@description, ''), description),
    default_time_range  = @default_time_range,
    default_granularity = @default_granularity
where id = @id and project_id = @project_id
returning *;

-- name: DeleteDashboard :one
delete from dashboards
where id = @id and project_id = @project_id
returning *;

-- name: CreateDashboardTile :one
insert into dashboard_tiles (id, dashboard_id, kind, view_mode, display_name, description, insight_query, markdown_body, layouts)
select @id, d.id, @kind, @view_mode, @display_name, @description, @insight_query, @markdown_body, @layouts
from dashboards d
where d.id = @dashboard_id and d.project_id = @project_id
returning *;

-- name: UpdateDashboardTile :one
update dashboard_tiles dt
set
  display_name  = coalesce(nullif(@display_name, ''), dt.display_name),
  description   = coalesce(nullif(@description, ''), dt.description),
  kind          = @kind,
  view_mode     = @view_mode,
  insight_query = @insight_query,
  markdown_body = @markdown_body,
  layouts       = @layouts
from dashboards d
where dt.id = @id
  and dt.dashboard_id = @dashboard_id
  and d.id = dt.dashboard_id
  and d.project_id = @project_id
returning dt.*;

-- name: DeleteDashboardTile :one
delete from dashboard_tiles dt
using dashboards d
where dt.id = @id
  and dt.dashboard_id = @dashboard_id
  and d.id = dt.dashboard_id
  and d.project_id = @project_id
returning dt.*;
