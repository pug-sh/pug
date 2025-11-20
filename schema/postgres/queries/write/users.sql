-- name: CreateUser :one
insert into users (id, project_id, external_id, metadata, segments)
values (@id, @project_id, @external_id, @metadata, '[]'::jsonb)
returning *;

-- name: UpdateUserMetadata :one
update users
set metadata = @metadata, update_time = now()
where id = @id
returning *;

-- name: UpdateUserSegments :one
update users
set segments = @segments, update_time = now()
where id = @id
returning *;

-- name: AddUserToSegment :one
update users
set segments = COALESCE((
    SELECT jsonb_agg(value)
    FROM (
        SELECT DISTINCT jsonb_array_elements_text(segments || @segment::jsonb) AS value
    ) t
), '[]'::jsonb), update_time = now()
WHERE id = @id
returning *;

-- name: RemoveUserFromSegment :one
update users
set segments = (
    SELECT jsonb_agg(value)
    FROM jsonb_array_elements(segments) as value
    WHERE value::text != @segment::text
), update_time = now()
where id = @id
returning *;

-- name: DeleteUser :exec
delete from users
where id = @id;
