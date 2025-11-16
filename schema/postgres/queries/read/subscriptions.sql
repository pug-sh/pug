-- name: GetSubscription :one
select * from subscriptions
where id = @id and project_id = @project_id;

-- name: GetSubscriptionByToken :one
select * from subscriptions
where token = @token;

-- name: GetSubscriptionsByProject :many
select * from subscriptions
where project_id = @project_id;

-- name: GetSubscriptionsByUser :many
select * from subscriptions
where user_id = @user_id;

-- name: GetActiveSubscriptionsByProject :many
select * from subscriptions
where project_id = @project_id and status = 'active';
