-- name: GetProfileByIDAndProjectID :one
select * from profiles
where id = @id and project_id = @project_id;

-- name: GetProfileByProjectAndExternalID :one
select * from profiles
where project_id = @project_id and external_id = @external_id::text limit 1;

-- name: GetProfilesByProjectID :many
select * from profiles
where project_id = @project_id;
