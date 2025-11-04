-- name: GetSubscription :one
SELECT * FROM subscriptions
WHERE id = @id AND project_id = @project_id;

-- name: GetSubscriptionByToken :one
SELECT * FROM subscriptions
WHERE token = @token;

-- name: GetSubscriptionsByProject :many
SELECT * FROM subscriptions
WHERE project_id = @project_id;

-- name: GetSubscriptionsByUser :many
SELECT * FROM subscriptions
WHERE user_id = @user_id;

-- name: GetActiveSubscriptionsByProject :many
SELECT * FROM subscriptions
WHERE project_id = @project_id AND status = 'active';