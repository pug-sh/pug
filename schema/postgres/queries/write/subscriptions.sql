-- name: CreateSubscription :one
INSERT INTO subscriptions (
    id, project_id, token, platform, metadata, status
) VALUES (
    @id, @project_id, @token, @platform, @metadata, @status
) RETURNING *;

-- name: GetSubscription :one
SELECT * FROM subscriptions
WHERE id = @id AND project_id = @project_id;

-- name: GetSubscriptionByToken :one
SELECT * FROM subscriptions
WHERE token = @token;

-- name: UpdateSubscriptionToken :one
UPDATE subscriptions
SET token = @token, update_time = now()
WHERE id = @id AND project_id = @project_id
RETURNING *;

-- name: UpdateSubscriptionStatus :one
UPDATE subscriptions
SET status = @status, update_time = now()
WHERE id = @id AND project_id = @project_id
RETURNING *;

-- name: UpdateSubscriptionMetadata :one
UPDATE subscriptions
SET metadata = @metadata, update_time = now()
WHERE id = @id AND project_id = @project_id
RETURNING *;

-- name: UpdateSubscriptionPlatform :one
UPDATE subscriptions
SET platform = @platform, update_time = now()
WHERE id = @id AND project_id = @project_id
RETURNING *;

-- name: UpdateSubscriptionHeartbeat :one
UPDATE subscriptions
SET last_heartbeat_time = now(), update_time = now()
WHERE id = @id AND project_id = @project_id
RETURNING *;

-- name: LinkSubscriptionToUser :one
UPDATE subscriptions
SET user_id = @user_id, update_time = now()
WHERE id = @id AND project_id = @project_id
RETURNING *;

-- name: UpdateSubscriptionUserId :one
UPDATE subscriptions
SET user_id = @user_id, update_time = now()
WHERE id = @id AND project_id = @project_id
RETURNING *;