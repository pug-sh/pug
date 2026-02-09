-- name: CreateSegment :one
insert into segments (description, display_name, filter, id, project_id)
values (@description, @display_name, @filter, @id, @project_id)
returning *;

-- name: DeleteSegment :exec
delete from segments
where id = @id;

-- name: GetActiveSegments :many
select * from segments
where project_id = @project_id and is_active = true;

-- name: GetSegment :one
select * from segments
where id = @id;

-- name: GetSegmentCountByProject :one
select count(*) from segments
where project_id = @project_id;

-- name: GetSegmentsByProject :many
select * from segments
where project_id = @project_id
order by create_time desc
limit @row_limit offset @row_offset;

-- name: UpdateSegment :one
update segments
set
  description = coalesce(nullif(@description, ''), description),
  display_name = coalesce(nullif(@display_name, ''), display_name),
  filter = coalesce(@filter, filter),
  is_active = coalesce(@is_active, is_active)
where id = @id
returning *;
