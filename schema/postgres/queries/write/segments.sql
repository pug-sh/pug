-- name: CreateSegment :one
INSERT INTO segments (
  id, project_id, name, description, filter
) VALUES (
  $1, $2, $3, $4, $5
) RETURNING *;

-- name: GetSegment :one
SELECT * FROM segments
WHERE id = $1;

-- name: GetSegmentsByProject :many
SELECT * FROM segments
WHERE project_id = $1
ORDER BY create_time DESC
LIMIT $2 OFFSET $3;

-- name: GetSegmentCountByProject :one
SELECT COUNT(*) FROM segments
WHERE project_id = $1;

-- name: UpdateSegment :one
UPDATE segments
SET
  name = COALESCE($2, name),
  description = COALESCE($3, description),
  filter = COALESCE($4, filter),
  is_active = COALESCE($5, is_active),
  update_time = NOW()
WHERE id = $1
RETURNING *;

-- name: DeleteSegment :exec
DELETE FROM segments
WHERE id = $1;

-- name: GetActiveSegments :many
SELECT * FROM segments
WHERE project_id = $1 AND is_active = true;