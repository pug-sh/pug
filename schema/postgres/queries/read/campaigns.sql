-- name: GetCampaignByID :one
select * from campaigns
where id = @id;

-- name: GetCampaignsByProjectID :many
select * from campaigns
where project_id = @project_id;

-- name: GetCampaignsByStatus :many
select * from campaigns
where status = @status;

-- name: GetScheduledCampaigns :many
select * from campaigns
where scheduled_time <= now() and status = 'scheduled';

-- name: GetCampaignById :one
select * from campaigns where id = @id;
