-- name: CreateCampaign :one
INSERT INTO campaigns (id, project_id, notification_data, scheduled_time, status)
VALUES (@id, @project_id, @notification_data, @scheduled_time, @status)
RETURNING *;

-- name: UpdateCampaign :one
UPDATE campaigns
SET notification_data = @notification_data, 
    scheduled_time = @scheduled_time,
    status = @status,
    update_time = now()
WHERE id = @id
RETURNING *;

-- name: UpdateCampaignStatus :one
UPDATE campaigns
SET status = @status, update_time = now()
WHERE id = @id
RETURNING *;

-- name: UpdateCampaignStartTime :one
UPDATE campaigns
SET start_time = @start_time, update_time = now()
WHERE id = @id
RETURNING *;

-- name: UpdateCampaignEndTime :one
UPDATE campaigns
SET end_time = @end_time, update_time = now()
WHERE id = @id
RETURNING *;

-- name: DeleteCampaign :exec
DELETE FROM campaigns
WHERE id = @id;