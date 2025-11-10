-- name: CreateUser :one
INSERT INTO users (id, project_id, external_id, metadata)
VALUES (@id, @project_id, @external_id, @metadata)
RETURNING *;

-- name: UpdateUserMetadata :one
UPDATE users
SET metadata = @metadata, update_time = now()
WHERE id = @id
RETURNING *;

-- name: DeleteUser :exec
DELETE FROM users
WHERE id = @id;