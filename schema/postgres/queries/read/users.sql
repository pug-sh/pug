-- name: GetUserByID :one
select id, create_time, external_id, metadata, project_id, segments, update_time from users
where id = @id;

-- name: GetUserByProjectAndExternalID :one
select id, create_time, external_id, metadata, project_id, segments, update_time from users
where project_id = @project_id and external_id = @external_id limit 1;

-- name: GetUsersByProjectID :many
select id, create_time, external_id, metadata, project_id, segments, update_time from users
where project_id = @project_id;

-- name: GetUserBySubscriptionID :one
select u.id, u.create_time, u.external_id, u.metadata, u.project_id, u.segments, u.update_time from users u
join subscriptions s on s.user_id = u.id
where s.id = @subscription_id;

-- name: GetUsersBySegment :many
select id, create_time, external_id, metadata, project_id, segments, update_time from users
where segments ? @segment::text;

-- name: GetUsersBySegments :many
select id, create_time, external_id, metadata, project_id, segments, update_time from users
where segments ?| @segments::text[];
