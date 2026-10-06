-- name: ListOpenProjectDeletions :many
-- Done rows stay listed while the purge job watches them for rows that came back.
select * from project_deletions
where status <> 'done' or done_at >= @watch_from
order by requested_at asc, project_id asc;

-- name: HasStalledProjectDeletions :one
-- Open a day after the request or the last round started: the pass is not
-- running, or keeps failing.
select exists (
  select 1 from project_deletions
  where status <> 'done'
    and coalesce(round_started_at, requested_at) < now() - interval '1 day'
);
