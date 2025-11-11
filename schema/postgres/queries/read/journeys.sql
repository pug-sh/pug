-- name: GetJourneysByProjectID :many
select * from journeys
where project_id = @project_id
order by create_time desc;
