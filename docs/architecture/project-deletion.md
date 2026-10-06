# Project deletion

> **Status: phases 1 to 3 implemented; the production CronJob, the orphan cleanup and phase 4 not yet.** Written 2026-10-05 for review.

## Summary

An org admin deletes a project. Three things follow.

1. **At once**, the project disappears from the app. Its API keys, public
   dashboard links and push credential stop working.
2. **An hour later**, a background job starts erasing everything the project
   stored, in Postgres and ClickHouse. At today's table sizes that takes minutes.
   A very large ClickHouse can take hours.
3. **For 30 days after that**, the job keeps checking that nothing came back.

A deleted project cannot be restored. The org keeps the usage the project
already counted, because that is a billing record.

It ships in four phases. Users see nothing until the last one. See "Phases".

## Before phase 1

`ProjectsService.Delete` is admin-only. No screen calls it yet.

It used to delete the `projects` row and let Postgres cascade. That had four
problems.

| Problem | Effect |
|---|---|
| ClickHouse was never touched. | Events, profiles and rollups stayed forever. Nothing could read them and nothing deleted them. |
| `usage_daily` cascaded. | The org's period count dropped. Deleting a project gave its events away and reset the quota. |
| The cascade ran inside the request. | A project with a million profiles was one long transaction. |
| Queued messages still landed. | The events and profile workers trust the message's project id, so anything queued before the delete was written after it. |

Phase 1 fixed the second and third. The first and fourth wait for phase 2.

## What gets deleted

"Request" is the RPC's own transaction. "Job" is the background job.

### Postgres

| Table | Holds | When | How |
|---|---|---|---|
| `api_keys` | API keys | request | delete |
| `dashboard_shares` | public dashboard links | request | delete |
| `campaigns` | push campaigns | request | delete |
| `projects.fcm_service_json` | the FCM service-account key | request | set to null |
| `profile_devices` | push tokens | job | batches of 5,000 |
| `profiles` | identified users and their traits | job | batches of 5,000 |
| `dashboards`, `dashboard_tiles` | dashboards | job, last step | cascade from `projects` |
| `compliance_requests` | erasure requests | job, last step | cascade from `projects` |
| `projects` | name, timezone | job, last step | delete |

The push stack (campaigns, devices, delivery) has no running worker today, so
`campaigns` and `profile_devices` are likely empty. They are deleted anyway.

### ClickHouse

Every table leads its sort key with `project_id`.

| Table | Holds | How |
|---|---|---|
| `events` | every event | delete |
| `profiles` | the profile read model | delete |
| `profile_aliases` | identity links | delete |
| `distinct_id_activity_states` | per-person activity, anonymous people included | delete |
| `dashboard_event_rollup_daily` | daily rollup | delete |
| `dashboard_session_rollup` | session rollup | delete |
| `event_names` | event names seen | delete |
| `property_keys_event_buckets` | property keys seen | delete |
| `property_keys_profile_current` | profile property keys | none. It rebuilds itself from `profiles` every 5 minutes. The job only checks it is empty. |

### Redis

| Key | Holds | Gone |
|---|---|---|
| `project:pubkey:*`, `project:prvkey:*` | the cached project row, FCM key included | at once, deleted after the commit as today |
| `propvalues:<project>:*` | property values for filter dropdowns | within 1 hour (TTL) |
| `filterschema:<project>` | the filter schema | within 5 minutes (TTL) |
| `cookieless:sess:<project>:*` | cookieless session ids | within 24 hours (TTL) |

The job starts an hour after the request, so the middle two have expired by
then. Deleting the cookieless keys early would need a scan of the whole
keyspace, so they expire instead.

## What stays

| Data | Why | Until |
|---|---|---|
| `usage_daily` rows | the org's billing record | the usage prune, at about 13 months |
| `usage_periods` rows | org totals, with no project column | kept |
| the `project_deletions` row | proof of the deletion, with no personal data | kept |
| NATS copies | Every stream keeps every message for 30 days, processed or not. Subjects carry no project id, so a deletion cannot pick them out. | 30 days |
| logs and traces | they carry the project id, not event data | telemetry retention |
| pug's own product analytics | when configured, the app sends the project's id and name as traits of the signed-in customer | kept |
| backups | the deployment's own | their rotation |

## How it works

### The request

`Delete` keeps its empty request and response. It runs one Postgres
transaction.

1. Lock the project row. If it is already being deleted, return `NotFound`.
   Only a concurrent delete gets this far. A later one fails at auth with
   `Unauthenticated`, because the `x-project-id` lookup skips the row. The old
   hard delete did the same.
2. Insert the `project_deletions` row as `pending`.
3. Delete the project's API keys, share links and campaigns.
4. Clear `fcm_service_json`.
5. Set `projects.deletion_time` to now.

After the transaction it drops the keys from the Redis cache, as the old delete
did. It does so even when the commit reports an error, because the commit may
still have landed. The tokens come from the delete itself (`returning token`).
That closes a gap in the old code: a key created between the token listing and
the delete was never dropped from the cache.

From then on every project query skips the row: the project list,
`x-project-id` auth and API-key lookups. So does the public share-link lookup,
which used to read only `dashboard_shares`. Without that, a share created while
the request runs would keep serving the project's data until the last step. The
name is free for a new project at once.

### The job

`pug cron purge` runs every 5 minutes, under an advisory lock, like
`pug cron usage`. Each pass moves every open deletion forward.

| Status | What the pass does | Next |
|---|---|---|
| `pending` | Waits until an hour after the request. Then counts the project's usage one last time. | `deleting` |
| `deleting` | Starts the ClickHouse deletes. Deletes Postgres rows in batches. When every table is empty, deletes the `projects` row. | `done` |
| `done` | For 30 days, re-counts the project's ClickHouse rows. If any came back, reopens it. | `deleting` |

Rules:

1. **Wait an hour.** The API-key cache lives an hour. After that nothing can
   resolve the project's keys, even if the cache invalidation was lost. Queued
   messages have drained too.
2. **ClickHouse deletes start together.** Every deletion past its hour starts
   its ClickHouse deletes in the same pass. Deletes queued together share one
   rewrite of each table, as "Why one delete per table" explains. Postgres
   batches still run one deletion at a time.
3. **Postgres in batches.** `profile_devices` first, then `profiles`, 5,000 rows
   per statement, each in its own transaction. Devices go first because their
   `profile_id` key is `on delete set null`. Deleting profiles first would
   rewrite every device row before deleting it. A pass spends at most two
   minutes, counted from its start, and continues on the next.
4. **The last step is one delete.**
   `delete from projects where id = ? and deletion_time is not null`. It
   cascades whatever is left: dashboards, tiles, erasure requests, and any row a
   late message inserted. The job can never delete a live project.

### ClickHouse deletes

The ClickHouse stage takes a filter and a list of tables. For each table it
checks two things: are there rows left, and is a delete for this project already
running.

| Rows left | Delete running | Action |
|---|---|---|
| yes | yes | wait |
| yes | no | `ALTER TABLE <t> DELETE WHERE project_id = ?` with `mutations_sync = 0` |
| no | yes | wait |
| no | no | the table is done |

"Delete running" comes from `system.mutations`, matched on the project id in the
command. The statement returns as soon as ClickHouse queues it, so a pass never
waits on ClickHouse.

The stage keeps no state of its own. `system.mutations` and the row counts are
its state. Any pass can call it any number of times, and it never queues a
second delete for the same table and project while one is running.

It refuses an empty project id. No code path issues a delete without
`project_id = ?`. It also refuses a project id that still has a live `projects`
row. That is the same guard as the last Postgres step. So no ledger row can
erase a live project's data, including one the operator command wrote.

It uses the heavyweight `ALTER TABLE … DELETE`, as GDPR erasure does. A
lightweight `DELETE FROM` only hides rows until a later merge, so it cannot
prove they are gone.

ClickHouse applies a table's deletes in the order they were queued. So a GDPR
erasure queued during a project delete cannot finish before it. The compliance
worker waits 5 minutes per delivery and queues its delete again on each of 5
deliveries. Then it marks the request failed. At today's sizes a project delete
takes minutes, so a later delivery gets through. An erasure that starts more
than about 25 minutes before a delete ends runs out of deliveries and fails. See
decision 6.

A table that still has rows after its delete finished gets another round.
Something wrote to it after the delete was queued. Each later round starts at
least an hour after the last one, and records an error. So does a round that
reopens a `done` deletion.

## Why one delete per table, not batches

ClickHouse stores rows in large, immutable data parts. It cannot remove a row in
place. A delete rewrites every part that holds a matching row, without those
rows.

So a delete costs the size of the parts it touches, not the number of rows it
removes. One delete per table rewrites each affected part once. Splitting it
into 1,000 batches would rewrite the same parts up to 1,000 times.

The same goes across projects. ClickHouse applies every delete waiting on a
part in a single rewrite. So deletes for several projects, queued together,
cost about one rewrite. One project at a time would rewrite the table once per
project. That is why the pass starts every due deletion at once.

The delete runs inside ClickHouse, in its background pool, beside normal
merges. Pug only queues it and checks on it later. That is what scales: the work
grows with the table, and none of it runs in a pug process. Queries and inserts
keep working meanwhile.

The cost is real. For a project that sent events every month, a delete rewrites
most of the `events` table's parts once. That is the price of physical deletion
in ClickHouse.

Postgres is the opposite. Deleting a row is cheap, but one huge transaction is
not. So Postgres rows go in batches.

## Why a table row, not a NATS message

The job is async either way. The question is what holds it.

1. **The row is written with the request.** It commits in the same transaction
   that hides the project. A NATS message would be a second write after that
   commit. If the publish failed, the project would be hidden with nothing
   queued to erase it. The admin could not retry, because the project is gone
   from their view.
2. **ClickHouse has no "delete finished" callback.** Something must poll
   `system.mutations` for minutes or hours. A NATS handler cannot hold a message
   that long. The first attempt at this feature tried, by blocking on
   `mutations_sync = 1`, and was scrapped for it.

So the row is the job, and one scheduled pass moves every job forward. That is
one moving part instead of three: a message, a worker and a poller.

## Late writes

The events, profile alias and profile upsert workers write ClickHouse without
checking the project. A message queued before the delete still lands after it.
Identify does check, through the `profiles` foreign key. Once the `projects` row
is gone it fails, retries for under a minute, and goes to the DLQ.

Three things cover this, with no change to the ingest path.

1. **The hour's wait.** Keys are dead by then. Workers retry for under a minute
   before a message goes to the DLQ. So queues have drained, unless a worker was
   down.
2. **Done means empty after every delete finished.** Rows that land mid-way get
   another round.
3. **The 30-day watch.** 30 days is the streams' `max_age`. Until then, a worker
   coming back from a long outage, or a consumer recreated with
   `deliver_policy: all`, could replay the project's messages. After it, no copy
   exists anywhere in NATS.

## Usage and billing

Usage counts belong to the org, not the project. Deleting a project must not
lower a period's count.

1. `usage_daily` drops its foreign key to `projects`. Its rows outlive the
   project. The org key stays.
2. The meter only touches rows of live projects. It neither updates nor
   reconciles away the days of a project being deleted or already gone. Its
   empty-read check counts only live projects' rows too. Otherwise, once the
   only project with events is deleted and erased, the meter reads nothing over
   days it has stored. It takes that for a bad read and refreshes no org, every pass, for up
   to a full period.
3. Before any delete, the job counts the project's events per day one last time,
   over its org's current period and the month to date. It uses the meter's query, filtered to
   the project, and stores the result in the same transaction that sets
   `deleting`. This runs once per deletion, because a row never goes back to
   `pending`. A reopen goes from `done` straight to `deleting`. Counting again
   then would overwrite the frozen days with only the late rows. A row without
   `org_id` skips the count. That project was deleted before this shipped, and
   its usage went with it.

After that the rows never change until the normal prune, and the org's period
total keeps them.

Without rule 2, the meter would delete the project's days as soon as ClickHouse
stopped returning them. That reconcile exists for GDPR erasure.

The usage page already labels an id it cannot name "Unknown project (id…)".
"Deleted project" would read better, as a copy change.

## Reuse for retention

Retention is not designed here. It can reuse two pieces of this design.

1. **The ClickHouse stage.** Project deletion passes `project_id = ?`. Retention
   would pass `project_id = ? and <time column> < ?`, over the tables that have
   one.
2. **The pass.** `pug cron purge` can run retention as a second task, held to
   once a day through `cron_state`, the way `pug cron usage` holds its prune.

What retention still has to decide:

| Table | Can it be cut by time? |
|---|---|
| `events` | yes, by `occur_time` |
| `dashboard_event_rollup_daily` | yes, by `day` |
| `property_keys_event_buckets` | yes, by `bucket_time` |
| `profiles`, `profile_aliases` | no. They hold current identity, not history. |
| `distinct_id_activity_states`, `dashboard_session_rollup`, `event_names` | no. They hold merged states, so they need a rebuild or a rule of their own. |
| `property_keys_profile_current` | rebuilds itself |

Two more facts for that design. Retention can differ per org, so one month's
partition mixes rows with different deadlines. Dropping partitions does not
work; deletes do. And retention cut-offs sit far outside the meter's window, so
retention never changes a counted period.

GDPR erasure could move onto the stage too. It still blocks on
`mutations_sync = 1`, which is what decision 6 is about.

## Schema

One Postgres migration, at the next free number when it merges. Everything in it
adds or relaxes, so old pods keep working during the rollout.

1. `projects.deletion_time timestamptz`. Null means live.
2. `projects_org_id_display_name_key` becomes a unique index with
   `where deletion_time is null`, under the same name. A new project can take a
   deleted project's name at once. The name check in `internal/core/projects`
   matches on that name, so it keeps working.
3. `usage_daily` drops `usage_daily_project_id_fkey`.
4. A new table, `project_deletions`.

| Column | Type | Meaning |
|---|---|---|
| `display_name` | `varchar(150)` | the project's name when deleted |
| `done_at` | `timestamptz` | when every store read empty |
| `error` | `text` | the last problem seen, for operators |
| `org_id` | `char(20)` | null only for a project deleted before this shipped |
| `project_id` | `char(20)`, primary key | no foreign key, so the row outlives the project |
| `requested_at` | `timestamptz` | |
| `requested_by` | `text` | `customer <id>`, or the operator's `--actor` |
| `round_started_at` | `timestamptz` | when the last ClickHouse round started |
| `rounds` | `integer` | ClickHouse rounds so far |
| `status` | `text` | `pending`, `deleting` or `done` |
| `update_time` | `timestamptz` | |

The Down half hard-deletes hidden projects (`deletion_time` set), and usage rows
whose project is gone. Without that, the full unique constraint and the foreign
key cannot come back.

## Operator command

`pug projects delete <project-id> --actor <who>` does what the RPC does, without
the admin check, like `pug billing` and `pug domains`.

It has two uses. A self-hosted operator can delete a project no admin can reach.
And it queues projects deleted before this shipped, which left their ClickHouse
rows behind. For an id with no `projects` row, the command only writes the
ledger row, and the job erases what ClickHouse holds. That row has no `org_id`,
so the job skips its usage count. Running the command on an id that already has
a row changes nothing while the deletion is open, and reopens a `done` one. A
reopen keeps the first requester. It refuses a malformed id, such as a truncated
paste. A well-formed wrong id still queues a deletion, which matches nothing.

Run it with the server's environment. It clears the API-key cache in the
server's Redis, and a checkout's `.env` would point it at a local one.

## Frontend (`../app`)

There is no delete button yet.

1. **Where.** Settings → General, at the bottom. Admins only, through `<Can>`.
   Demo sessions never see Settings.
2. **Confirm.** The admin types the project's name. The button stays disabled
   until it matches. This is a new pattern in the app, for its only action that
   erases data for good.
3. **Copy.** "Delete {name}. This erases the project's events, profiles,
   dashboards and API keys. It can't be undone. Events already counted stay on
   your organization's usage."
4. **After.** Refetch the project list, drop the project from
   `pug:lastProjectByOrg`, and go to the first remaining project. That key holds
   one project per org, here the deleted one, so there is no other last visit
   to return to. With none left, show the existing "No projects yet" screen.

A follow-up for other tabs and other admins. Today they keep the deleted project
until they reload, and each failed call costs a session refresh. Polling pages
repeat that every 5 to 10 seconds. The backend can tag the "project not found or
access denied" error with `PROJECT_NOT_FOUND`, which is additive. The app can
then refetch the project list instead of refreshing the session.

## Cloud and self-hosted

| | Cloud | Self-hosted |
|---|---|---|
| Delete button and RPC | same | same |
| The job | a CronJob runs `pug cron purge` every 5 minutes | the operator schedules `pug cron purge`, like `pug cron usage` |
| If the job never runs | | The project is hidden and its keys are dead, but its data stays. `project_deletions` shows it `pending`, and the server logs an error at every start. |
| Usage rows | kept and billed | kept. Billing is off, so they only feed the usage page. |
| Operator command | support deletes on request | the operator deletes without an org admin |

## Failures and alerts

The pass logs and records errors like `pug cron usage`, and exits non-zero when a
step failed. It records an error when:

1. a ClickHouse delete reports a failure in `system.mutations`. ClickHouse
   retries it on its own.
2. a new round starts, because rows came back.
3. a deletion is still not `done` a day after its last round started. The
   error names the tables it is waiting on.

These all come from the pass, so none fires when the pass never runs. The server
covers that case. At startup it logs an error while any deletion is still open a
day after its request or its last round started. That catches a job that never
ran, and one that stopped after a deletion's first round.

## Phases

Four phases. Phase 2 ships in three parts, and 2a and 2b share one PR. No screen calls
`Delete` until phase 4, so phases 1 to 3 change nothing a user can see.

| Phase | Ships | Afterwards |
|---|---|---|
| 1. Hide on delete | the migration and the request | `Delete` hides the project and keeps its usage. Its data waits in `pending`. |
| 2a. The ClickHouse stage | the ClickHouse deletes | nothing calls them yet |
| 2b. The job's steps | the usage freeze, the Postgres batches, the last step and the watch | nothing runs them yet |
| 2c. The pass | `pug cron purge`, its image and its CronJob | pending deletions are erased |
| 3. Operator command | `pug projects delete` and the orphan cleanup | projects deleted before this shipped are erased too |
| 4. The button | the delete section in `../app` | admins can delete a project |

### Phase 1: hide on delete

| Where | Change |
|---|---|
| `schema/postgres/migrations` | the migration in "Schema" |
| `schema/postgres/queries/read/projects.sql` | every query adds `deletion_time is null` |
| `schema/postgres/queries/read/dashboards.sql` | `GetEnabledDashboardShareByToken` joins `projects` and skips deleted ones |
| `schema/postgres/queries/read/usage.sql` | `CountUsageDailyInRange` counts only live projects' rows |
| `schema/postgres/queries/write/projects.sql` | `DeleteProject` becomes the request's statements; the two updates add the filter |
| `schema/postgres/queries/write/usage.sql` | the upsert and the reconcile skip projects that are not live |
| `internal/core/projects` | `DeleteProject` becomes one transaction, with the tokens from `returning token` |
| `docs/architecture/usage.md` | the meter skips projects that are not live |

Tests:

1. **The request.** The project vanishes from every project query. Its keys stop
   resolving and its share links 404, including a share row inserted after the
   request. The FCM key is cleared. Its name is free. A second delete fails at
   auth with `Unauthenticated`, and a concurrent one returns `NotFound`.
2. **Usage.** The meter neither updates nor drops a hidden project's days. With
   only a hidden project's rows stored and nothing in ClickHouse, a pass
   refreshes every org as idle.

The migration runs with the release. Everything in it adds or relaxes, so old
pods keep working during the rollout. The meter's new filters must be live
before the first deletion, so `cron-usage` moves to the same release.

Afterwards a deletion hides the project, revokes its keys, links and push
credential, and keeps its usage. Its data waits in `pending` for phase 2. Only a
direct API call can get there, since no screen calls `Delete` yet.

### Phase 2a: the ClickHouse stage

| Where | Change |
|---|---|
| `internal/core/purge` (new) | the ClickHouse stage and its table list |

Tests:

1. **Inventory guard.** The stage's tables are every ClickHouse table with a
   `project_id` column, views aside. A new table cannot be forgotten silently.
2. **Stage.** At most one queued delete per table and project. Project B's
   delete is queued while project A's is running. An empty project id, or one
   with a live `projects` row, is refused.

### Phase 2b: the job's steps

| Where | Change |
|---|---|
| `schema/postgres/queries/write/usage.sql` | a freeze upsert for the job |
| `schema/postgres/queries/{read,write}/project_deletions.sql` | the job's reads, status updates and batches |
| `internal/core/usage` | the meter's query, filtered to one project |
| `internal/core/purge` | the job's steps |

Tests:

1. **Inventory guard.** Every Postgres table with a `project_id` column cascades
   from `projects`, or is on a short keep list (`usage_daily`,
   `project_deletions`).
2. **End to end.** Two projects with rows in every table. Delete one and run
   passes on a moved clock. The deleted one is gone everywhere. The other is
   untouched, row for row.
3. **Late write.** A row inserted mid-delete gets a second round. The deletion
   then finishes, with an error recorded.
4. **Watch.** A row inserted after `done` reopens the deletion.
5. **Usage.** The pass counts the project's days once, before any delete. The
   frozen days survive the last step and sum into the org's total. A reopened
   deletion does not count again. Phase 1's tests cover the meter's side.
6. **Stuck.** A deletion not `done` a day after its last round started records
   an error.
7. **Live project.** A deletion row for a live project erases nothing, and does
   not hold up another deletion.
8. **Failing delete.** A ClickHouse delete that keeps failing is recorded.

### Phase 2c: the pass

| Where | Change |
|---|---|
| `internal/app/cron/purge`, `cmd/cron/purge` (new) | the pass: lock, root span, timeout, exit code. `cron.JobPurge` joins the lock keys. |
| `internal/app/server` | a startup check that logs an error for any deletion stuck over a day |
| `cmd/pug` | `pug cron purge`. `pug dev` lists it as not scheduled, like `pug cron usage`. |
| `.github/workflows/release.yaml`, `Makefile` | a `cron-purge` image beside `cron-usage` |
| `CLAUDE.md` | `pug cron purge` beside `pug cron usage` |

The pass starts its own root span. Without one, `telemetry.RecordError` does
nothing in a one-shot binary. The pass times out after 3 minutes.

In production a CronJob copied from `cron-usage` runs it every 5 minutes, with
`concurrencyPolicy: Forbid` and `activeDeadlineSeconds: 270`. A stuck Job is
then gone before the next run. Usage's 3000 seconds suits an hourly schedule,
not this one.

Tests:

1. **Never scheduled.** A deletion `pending` for over a day makes the server log
   an error at startup. So does one left `deleting` a day after its last round.
2. **Failed pass.** A pass that fails exits non-zero.

Until a deletion exists, each pass finds nothing to do. A deletion made through
the API in phase 1 is erased by the first pass.

### Phase 3: operator command and orphans

| Where | Change |
|---|---|
| `schema/postgres/queries/write/projects.sql` | a lock by id alone, and the ledger row for an id with no `projects` row |
| `internal/core/projects` | the operator's delete, sharing the request's statements |
| `internal/app/projects` (new), `cmd/pug` | `pug projects delete`, as in "Operator command" |
| `CLAUDE.md` | `pug projects delete` beside `pug billing` and `pug domains` |

Tests:

1. **Operator command.** A live project's id is hidden and queued, as by the
   RPC. An id with no `projects` row gets only a ledger row, with no `org_id`,
   and the job skips its usage count. A second run changes nothing while the
   deletion is open, and reopens a `done` one. A blank `--actor` is refused.

Then, in production:

1. Find the orphans: project ids in ClickHouse `events` that Postgres does not
   know, found with a `SELECT`.
2. Queue each with `pug projects delete`, a few dozen at a time. Each open
   deletion costs about 10 ClickHouse queries a pass, `done` ones included for
   30 days, and they share the pass's 2-minute budget.
3. Watch them reach `done`, and time the ClickHouse deletes. Decision 6 assumes
   they take minutes.

That way the job's first real deletes are of data no customer can see.

### Phase 4: the button

| Where | Change |
|---|---|
| `../app`, Settings → General | the delete section in "Frontend" |
| `../app`, usage page | "Deleted project" instead of "Unknown project (id…)" |

Tests:

1. **Delete section.** Only admins see it. The button stays disabled until the
   typed name matches. After a delete the app lands on the first remaining
   project, or on "No projects yet".

Ship it once phase 3's deletions are `done` and the CronJob has run cleanly
since phase 2c. The `PROJECT_NOT_FOUND` follow-up in "Frontend" can come later.

## Not in this design

1. Retention. See "Reuse for retention".
2. Restoring a deleted project.
3. Deleting an org or an account. Neither exists.
4. Removing a project's NATS copies. That needs the project id in subjects.

## Decisions for you

1. **Usage counts stay.** Recommended. They are the org's billing record.
   Deleting them un-bills the project's events and resets the quota.
2. **The erasure ledger goes with the project.** Recommended. Nobody can read it
   once the project is gone, and it holds the ids of people who asked to be
   forgotten. The deletion record covers them. This reverses a note from the
   scrapped first attempt, which kept it.
3. **No restore window.** Recommended for now. The hour's wait could become one
   later. A restore inside it would only lose what the request deleted: API
   keys, share links, campaigns and the FCM key.
4. **NATS copies age out in 30 days.** Recommended for now. Erasure has the same
   gap today. The fix is the project id in subjects, so a deletion can purge by
   subject.
5. **The last project can be deleted.** Recommended. The app then shows "No
   projects yet". Org settings are unreachable without a project, but that gap
   exists today.
6. **Erasure can wait behind a long delete.** Recommended for now: accept it.
   Today's deletes take minutes, inside the compliance worker's roughly 25
   minutes of retries. Move erasure onto the ClickHouse stage before a delete
   can take longer. Then it no longer blocks on `mutations_sync = 1`.
