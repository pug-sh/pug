-- name: CreateCampaign :one
insert into campaigns (id, name, project_id, notification_data, scheduled_time, status)
values (@id, @name, @project_id, @notification_data, @scheduled_time, @status)
returning *;

-- name: UpdateCampaign :one
update campaigns
set name = @name,
    notification_data = @notification_data, 
    scheduled_time = @scheduled_time,
    status = @status,
    update_time = now()
where id = @id
returning *;

-- name: UpdateCampaignStatus :one
update campaigns
set status = @status, update_time = now()
where id = @id
returning *;

-- name: UpdateCampaignStartTime :one
update campaigns
set start_time = @start_time, update_time = now()
where id = @id
returning *;

-- name: UpdateCampaignEndTime :one
update campaigns
set end_time = @end_time, update_time = now()
where id = @id
returning *;

-- name: DeleteCampaign :exec
delete from campaigns
where id = @id and project_id = @project_id;
