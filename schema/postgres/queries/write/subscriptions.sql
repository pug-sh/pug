-- name: CreateSubscription :one
insert into subscriptions (
    id, project_id, token, platform, metadata, status, updater, user_id
) values (
    @id, @project_id, @token, @platform, @metadata, @status, @updater, @user_id
) returning *;

-- name: GetSubscription :one
select * from subscriptions
where id = @id and project_id = @project_id;

-- name: GetSubscriptionByToken :one
select * from subscriptions
where token = @token;

-- name: UpdateSubscriptionToken :one
update subscriptions
set token = @token, update_time = now()
where id = @id and project_id = @project_id
returning *;

-- name: UpdateSubscriptionStatus :one
update subscriptions
set status = @status, update_time = now()
where id = @id and project_id = @project_id
returning *;

-- name: UpdateSubscriptionMetadata :one
update subscriptions
set metadata = @metadata, update_time = now()
where id = @id and project_id = @project_id
returning *;

-- name: UpdateSubscriptionPlatform :one
update subscriptions
set platform = @platform, update_time = now()
where id = @id and project_id = @project_id
returning *;

-- name: LinkSubscriptionToUser :one
update subscriptions
set user_id = @user_id, update_time = now()
where id = @id and project_id = @project_id
returning *;

-- name: UpdateSubscriptionUserID :one
update subscriptions
set user_id = @user_id, update_time = now()
where id = @id and project_id = @project_id
returning *;

-- name: UpdateSubscriptionUpdater :one
update subscriptions
set updater = @updater, update_time = now()
where id = @id and project_id = @project_id
returning *;

-- name: UpdateSubscriptionHeartbeat :one
update subscriptions
set last_heartbeat_time = now(), update_time = now()
where id = @id and project_id = @project_id
returning *;
