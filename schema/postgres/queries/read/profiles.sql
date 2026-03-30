-- name: GetProfileByIDAndProjectID :one
select * from profiles
where id = @id and project_id = @project_id;

-- name: GetProfileByProjectAndExternalID :one
select * from profiles
where project_id = @project_id and external_id = @external_id::text limit 1;

-- name: GetProfilesByProjectID :many
select * from profiles
where project_id = @project_id
  and (
    @has_cursor::bool = false
    or create_time < @cursor_time
    or (create_time = @cursor_time and id < @cursor_id)
  )
order by create_time desc, id desc
limit @page_size;

-- name: GetProfilePropertyKeys :many
select distinct key
from profiles,
     jsonb_object_keys(properties) as key
where project_id = @project_id
order by key asc
limit 1000;
