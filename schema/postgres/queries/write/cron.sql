-- name: TryCronLock :one
-- Transaction-scoped so the lock dies with its transaction. A session lock taken
-- through a pooled connection can be released on a different session, or never.
select pg_try_advisory_xact_lock(@lock_key::bigint) as acquired;

-- name: MarkCronRun :exec
insert into cron_state (task, last_run_at)
values (@task, @last_run_at)
on conflict (task) do update set last_run_at = excluded.last_run_at;
