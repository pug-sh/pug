-- name: GetProfileByIDAndProjectID :one
select * from profiles
where id = @id and project_id = @project_id and deletion_time is null;

-- name: GetProfileByProjectAndExternalID :one
select * from profiles
where project_id = @project_id and external_id = @external_id::text and deletion_time is null limit 1;

-- name: GetProfilesByProjectID :many
select * from profiles
where project_id = @project_id
  and deletion_time is null
  and (
    @has_cursor::bool = false
    or create_time < @cursor_time
    or (create_time = @cursor_time and id < @cursor_id)
  )
order by create_time desc, id desc
limit @page_size;

-- name: GetAllProfilesByProjectID :many
-- This query is only for seeding ClickHouse. Do not use in application code.
select id, external_id, properties, create_time, update_time
from profiles
where project_id = @project_id and deletion_time is null;
