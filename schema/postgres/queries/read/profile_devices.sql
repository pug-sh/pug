-- name: GetProfileDevicesByProfileID :many
select * from profile_devices
where profile_id = @profile_id;

-- name: GetActiveProfileDevicesByProject :many
select * from profile_devices
where project_id = @project_id and status = 'active';
