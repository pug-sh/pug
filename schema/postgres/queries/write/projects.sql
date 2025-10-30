-- name: CreateProject :one
insert into projects (api_key, customer_id, create_time, display_name, fcm_service_json, id, update_time)
values (@api_key, @customer_id, now(), @display_name, @fcm_service_json, @id, now())
returning *;
