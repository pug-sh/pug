-- name: GetUserByID :one
select id, create_time, external_id, properties, custom_properties, project_id, update_time from users
where id = @id;

-- name: GetUserByProjectAndExternalID :one
select id, create_time, external_id, properties, custom_properties, project_id, update_time from users
where project_id = @project_id and external_id = @external_id limit 1;

-- name: GetUsersByProjectID :many
select id, create_time, external_id, properties, custom_properties, project_id, update_time from users
where project_id = @project_id;

-- name: GetUserBySubscriptionID :one
select u.id, u.create_time, u.external_id, u.properties, u.custom_properties, u.project_id, u.update_time from users u
join subscriptions s on s.user_id = u.id
where s.id = @subscription_id;
