-- name: GetCronState :one
select * from cron_state where task = @task;
