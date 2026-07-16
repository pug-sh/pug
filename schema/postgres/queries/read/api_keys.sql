-- name: GetApiKeysByProjectID :many
select * from api_keys where project_id = @project_id order by create_time asc, id asc;

-- name: GetApiKeyTokensByProjectID :many
-- Just the cache keys: invalidating a project's cached row means deleting the
-- entry under every token that resolves to it.
select token from api_keys where project_id = @project_id;
