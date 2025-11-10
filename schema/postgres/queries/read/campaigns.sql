-- name: GetCampaignByID :one
SELECT * FROM campaigns
WHERE id = @id;

-- name: GetCampaignsByProjectID :many
SELECT * FROM campaigns
WHERE project_id = @project_id;

-- name: GetCampaignsByStatus :many
SELECT * FROM campaigns
WHERE status = @status;

-- name: GetScheduledCampaigns :many
SELECT * FROM campaigns
WHERE scheduled_time <= now() AND status = 'scheduled';