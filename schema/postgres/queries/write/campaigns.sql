-- name: CreateCampaign :one
insert into campaigns (id, name, notification_data, project_id, scheduled_time, status)
values (@id, @name, @notification_data, @project_id, @scheduled_time, @status)
returning *;

-- name: DeleteCampaign :exec
delete from campaigns
where id = @id and project_id = @project_id;

-- name: UpdateCampaign :one
update campaigns
set name = coalesce(nullif(@name, ''), name),
    notification_data = coalesce(nullif(@notification_data, ''), notification_data),
    scheduled_time = coalesce(@scheduled_time, scheduled_time)
where id = @id and project_id = @project_id
returning *;

-- name: UpdateCampaignEndTime :one
update campaigns
set end_time = @end_time
where id = @id
returning *;

-- name: UpdateCampaignStartTime :one
update campaigns
set start_time = @start_time
where id = @id
returning *;

-- name: UpdateCampaignStatus :one
update campaigns
set status = @status
where id = @id
returning *;
