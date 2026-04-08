-- name: SaveProfileDevice :one
insert into profile_devices (id, platform, profile_id, project_id, properties, status, token)
values (@id, @platform, @profile_id, @project_id, coalesce(@properties::jsonb, '{}'), @status, nullif(@token, ''))
on conflict (project_id, id) do update set
  platform = excluded.platform,
  profile_id = coalesce(excluded.profile_id, profile_devices.profile_id),
  properties = jsonb_shallow_merge(profile_devices.properties, excluded.properties),
  status = excluded.status,
  token = coalesce(nullif(excluded.token, ''), profile_devices.token)
returning *;

-- name: LinkDeviceToProfile :execrows
-- Assigns a device to a profile. Always overwrites — handles both first-time
-- linking (NULL → profile) and account switching (old profile → new profile).
-- Idempotent: 0 rows if device doesn't exist.
update profile_devices
set profile_id = @profile_id
where id = @device_id and project_id = @project_id;

-- name: UpdateProfileDeviceStatus :one
update profile_devices
set status = @status
where id = @id and project_id = @project_id
returning *;

-- name: DeactivateDevicesByProfileID :execrows
update profile_devices
set status = 'inactive'
where profile_id = @profile_id and project_id = @project_id and status = 'active';

-- name: UpdateProfileDeviceToken :one
update profile_devices
set token = @token
where id = @id and project_id = @project_id
returning *;
