-- name: GetProfileDevice :one
select * from profile_devices
where id = @id and project_id = @project_id;

-- name: GetProfileDevicesByProfileID :many
select * from profile_devices
where profile_id = @profile_id;

-- name: GetProfileDevicesByProject :many
select * from profile_devices
where project_id = @project_id;

-- name: GetActiveProfileDevicesByProject :many
select * from profile_devices
where project_id = @project_id and status = 'active';
