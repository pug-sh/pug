-- name: GetUserByID :one
SELECT * FROM users
WHERE id = @id;

-- name: GetUserByProjectAndExternalID :one
SELECT * FROM users
WHERE project_id = @project_id AND external_id = @external_id;

-- name: GetUsersByProjectID :many
SELECT * FROM users
WHERE project_id = @project_id;