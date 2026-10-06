-- name: ListOpenProjectDeletions :many
-- Done rows stay listed while the purge job watches them for rows that came back.
select * from project_deletions
where status <> 'done' or done_at >= @watch_from
order by requested_at asc, project_id asc;
