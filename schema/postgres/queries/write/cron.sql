-- name: TryCronLock :one
-- Transaction-scoped so the lock dies with its transaction. A session-scoped lock
-- outlives the call that took it, which through a pool cuts both ways: an unlock
-- routed to a different connection silently fails (postgres only lets the holding
-- session release it), so the lock rides its original connection back into the
-- pool never released -- and a later caller handed that same connection can
-- release a lock it never took.
select pg_try_advisory_xact_lock(@lock_key::bigint) as acquired;

-- name: MarkCronRun :exec
insert into cron_state (task, last_run_at)
values (@task, @last_run_at)
on conflict (task) do update set last_run_at = excluded.last_run_at;
