-- name: GetProfileDevicesByProfileID :many
select * from profile_devices
where profile_id = @profile_id and project_id = @project_id;

-- name: GetActiveProfileDevicesByProject :many
-- Excludes devices still linked to a soft-deleted profile so campaigns
-- never target a "deleted" user's devices.
select pd.* from profile_devices pd
left join profiles p on p.id = pd.profile_id and p.project_id = pd.project_id
where pd.project_id = @project_id
  and pd.status = 'active'
  and pd.id > @after_id
  and (pd.profile_id is null or p.deletion_time is null)
order by pd.id
limit @row_limit;
