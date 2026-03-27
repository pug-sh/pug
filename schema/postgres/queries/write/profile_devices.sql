-- name: SaveProfileDevice :one
insert into profile_devices (id, platform, profile_id, project_id, properties, status, token)
values (@id, @platform, @profile_id, @project_id, coalesce(@properties::jsonb, '{}'), @status, nullif(@token, ''))
on conflict (project_id, id) do update set
  platform = excluded.platform,
  properties = jsonb_shallow_merge(profile_devices.properties, excluded.properties),
  status = excluded.status,
  token = coalesce(nullif(excluded.token, ''), profile_devices.token)
returning *;

-- name: UpdateProfileDeviceStatus :one
update profile_devices
set status = @status
where id = @id and project_id = @project_id
returning *;

-- name: UpdateProfileDeviceToken :one
update profile_devices
set token = @token
where id = @id and project_id = @project_id
returning *;
