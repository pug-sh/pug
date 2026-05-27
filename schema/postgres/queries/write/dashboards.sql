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
insert into dashboard_tiles (
  id, dashboard_id, kind, view_mode, display_name, description,
  insight_query, markdown_body, layouts,
  compare, thresholds, header, visualization, payload_hash
)
select @id, d.id, @kind, @view_mode, @display_name, @description,
       @insight_query, @markdown_body, @layouts,
       @compare, @thresholds, @header, @visualization, @payload_hash
from dashboards d
where d.id = @dashboard_id and d.project_id = @project_id
returning *;

-- name: UpsertDashboardTileUpdate :execrows
-- Full-replace update gated on payload_hash. If the stored hash matches the
-- caller's hash, zero rows are touched and update_time stays put. Existence /
-- ownership is validated by the caller via the prior tile-id load; the join
-- onto dashboards is defense-in-depth.
update dashboard_tiles dt
set
  display_name  = @display_name,
  description   = @description,
  kind          = @kind,
  view_mode     = @view_mode,
  insight_query = @insight_query,
  markdown_body = @markdown_body,
  layouts       = @layouts,
  compare       = @compare,
  thresholds    = @thresholds,
  header        = @header,
  visualization = @visualization,
  payload_hash  = @payload_hash
from dashboards d
where dt.id = @id
  and dt.dashboard_id = @dashboard_id
  and d.id = dt.dashboard_id
  and d.project_id = @project_id
  and dt.payload_hash <> @payload_hash;

-- name: DeleteDashboardTilesNotIn :execrows
-- Deletes every tile on the dashboard whose id is not in keep_ids. Used by
-- Upsert to remove tiles the client dropped from its draft.
delete from dashboard_tiles dt
using dashboards d
where dt.dashboard_id = @dashboard_id
  and d.id = dt.dashboard_id
  and d.project_id = @project_id
  and dt.id <> all(@keep_ids::char(20)[]);

-- name: UpsertDashboardMetadata :execrows
-- Full-replace metadata write gated on (tiles_changed OR metadata changed).
-- If neither, zero rows are touched and update_time stays put. If tiles_changed
-- is true but metadata is identical, the row is still UPDATEd (with the same
-- values) so the moddatetime trigger bumps dashboard.update_time.
update dashboards
set display_name        = @display_name,
    description         = @description,
    default_time_range  = @default_time_range,
    default_granularity = @default_granularity
where id = @id and project_id = @project_id
  and (
    @tiles_changed::bool
    or (display_name, description, default_time_range, default_granularity)
       is distinct from
       (@display_name::varchar(150), @description::text, @default_time_range::text, @default_granularity::text)
  );
