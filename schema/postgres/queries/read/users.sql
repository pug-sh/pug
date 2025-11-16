-- name: GetUserByID :one
select * from users
where id = @id;

-- name: GetUserByProjectAndExternalID :one
select * from users
where project_id = @project_id and external_id = @external_id limit 1;

-- name: GetUsersByProjectID :many
select * from users
where project_id = @project_id;

-- name: GetUserBySubscriptionID :one
select u.* from users u
join subscriptions s on s.user_id = u.id
where s.id = @subscription_id;
