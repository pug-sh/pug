-- name: GetSubscription :one
select * from subscriptions
where id = @id and project_id = @project_id;

-- name: GetSubscriptionByToken :one
select * from subscriptions
where token = @token;

-- name: GetSubscriptionsByProject :many
select * from subscriptions
where project_id = @project_id;

-- name: GetSubscriptionsByProfile :many
select * from subscriptions
where profile_id = @profile_id;

-- name: GetActiveSubscriptionsByProject :many
select * from subscriptions
where project_id = @project_id and status = 'active';
