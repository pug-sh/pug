-- name: CreateJourney :one
insert into journeys (
  config,
  create_time,
  description,
  end_time,
  entry_type,
  id,
  name,
  project_id,
  start_time,
  state,
  update_time
) values (
  @config,
  now(),
  @description,
  @end_time,
  @entry_type,
  @id,
  @name,
  @project_id,
  @start_time,
  @state,
  now()
) returning *;
