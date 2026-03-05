-- name: GetProfileDevicesByProfileID :many
select * from profile_devices
where profile_id = @profile_id and project_id = @project_id;

-- name: GetActiveProfileDevicesByProject :many
select * from profile_devices
where project_id = @project_id and status = 'active' and id > @after_id
order by id
limit @row_limit;
