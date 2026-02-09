-- name: CreateSubscription :one
insert into subscriptions (id, metadata, platform, profile_id, project_id, status, token, updater)
values (@id, @metadata, @platform, @profile_id, @project_id, @status, @token, @updater)
returning *;

-- name: GetSubscription :one
select * from subscriptions
where id = @id and project_id = @project_id;

-- name: GetSubscriptionByToken :one
select * from subscriptions
where token = @token;

-- name: LinkSubscriptionToProfile :one
update subscriptions
set profile_id = @profile_id
where id = @id and project_id = @project_id
returning *;

-- name: UpdateSubscriptionHeartbeat :one
update subscriptions
set last_heartbeat_time = now()
where id = @id and project_id = @project_id
returning *;

-- name: UpdateSubscriptionMetadata :one
update subscriptions
set metadata = @metadata
where id = @id and project_id = @project_id
returning *;

-- name: UpdateSubscriptionPlatform :one
update subscriptions
set platform = @platform
where id = @id and project_id = @project_id
returning *;

-- name: UpdateSubscriptionProfileID :one
update subscriptions
set profile_id = @profile_id
where id = @id and project_id = @project_id
returning *;

-- name: UpdateSubscriptionStatus :one
update subscriptions
set status = @status
where id = @id and project_id = @project_id
returning *;

-- name: UpdateSubscriptionToken :one
update subscriptions
set token = @token
where id = @id and project_id = @project_id
returning *;

-- name: UpdateSubscriptionUpdater :one
update subscriptions
set updater = @updater
where id = @id and project_id = @project_id
returning *;
