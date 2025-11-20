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

-- name: GetActiveSegments :many
SELECT * FROM segments
WHERE project_id = $1 AND is_active = true;