-- name: GetCampaignByID :one
select * from campaigns where id = @id;

-- name: GetCampaignByIDAndProjectID :one
select * from campaigns where id = @id and project_id = @project_id;

-- name: GetCampaignsByProjectID :many
select * from campaigns where project_id = @project_id;

-- name: GetScheduledCampaigns :many
select * from campaigns where scheduled_time <= now() and status = 'scheduled';
