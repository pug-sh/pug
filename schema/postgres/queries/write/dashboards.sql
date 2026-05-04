-- name: CreateDashboard :one
insert into dashboards (description, display_name, id, project_id)
values (@description, @display_name, @id, @project_id)
returning *;

-- name: UpdateDashboardDisplayName :one
update dashboards
set display_name = @display_name,
    description = @description
where id = @id and project_id = @project_id
returning *;

-- name: DeleteDashboard :one
delete from dashboards
where id = @id and project_id = @project_id
returning *;

-- name: CreateDashboardInsight :one
insert into dashboard_insights (dashboard_id, description, display_name, id, insight_query, layouts)
select d.id, @description, @display_name, @id, @insight_query, @layouts
from dashboards d
where d.id = @dashboard_id and d.project_id = @project_id
returning *;

-- name: UpdateDashboardInsight :one
update dashboard_insights di
set
  description = @description,
  display_name = @display_name,
  insight_query = @insight_query,
  layouts = @layouts
from dashboards d
where di.id = @id
  and di.dashboard_id = @dashboard_id
  and d.id = di.dashboard_id
  and d.project_id = @project_id
returning di.*;

-- name: DeleteDashboardInsight :one
delete from dashboard_insights di
using dashboards d
where di.id = @id
  and di.dashboard_id = @dashboard_id
  and d.id = di.dashboard_id
  and d.project_id = @project_id
returning di.*;
