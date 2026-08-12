# Event Usage Metering

Detailed reference for event usage metering (`internal/core/usage`,
`internal/app/cron`, `proto/dashboard/usage`). Linked from the root
[`CLAUDE.md`](../../CLAUDE.md) — read this when working on the meter, the
`cron_state` job scaffolding, or the `GetUsage` read.

How many events has an org sent this month? Pug answers that from a small
scheduled meter rather than from an analytical query on every page load.

The subsystem is deliberately narrow: it counts, stores and serves. It does not
price, cap, throttle or reject anything.

## 1. Structural invariants

Three properties everything below preserves.

1. **Ingestion never consults usage.** The meter reads ClickHouse well after
   ingestion has committed. No event is rejected, throttled, delayed or dropped
   because of a count, and no ingestion path imports `internal/core/usage`.
2. **The meter is optional.** A deployment that never schedules `pug cron usage`
   has no numbers, and nothing else degrades. `GetUsage` reports an absent
   `usage_computed_at`, which the client renders as "unknown" — never as zero.
3. **Counts are reporting, not entitlement.** Nothing in pug branches on a usage
   number. There is no quota column, no limit constant and no over-limit state,
   because there is no tier for one to belong to.

## 2. What gets counted

`uniqExact(event_id)` over the raw `events` table, grouped by `(project_id, day)`.

Three deliberate choices, each of which has a tempting wrong alternative:

- **`uniqExact`, never `uniq`.** `uniq` is a HyperLogLog estimate with ~0.5–2%
  error. A count a customer can compare against their own SDK's send count must
  be exact.
- **Raw `events`, never `dashboard_event_rollup_daily`.** The rollup's key omits
  `event_id`, so a redelivered event is indistinguishable from a second event and
  the drift accumulates monotonically. Reading raw costs more and is correct.
- **No `FINAL`.** `events` is a `ReplacingMergeTree`, so the same `event_id` may
  physically exist several times before a merge collapses it — but `uniqExact`
  counts distinct ids, which is the same number before and after that merge.
  Paying for `FINAL` would buy nothing.

The scan is bounded by the query's own time window, and `project_id` leads the
`events` primary key, so the optional project filter prunes granules rather than
scanning every partition.

## 3. Storage

Two tables (migration `018_create_usage.sql`):

- **`usage_daily`** — `(project_id, day)` primary key, `org_id` denormalized,
  `event_count`. The grain is what makes one metering pass serve every org, what
  bounds `uniqExact`'s memory, and what the dashboard charts.
- **`usage_periods`** — `(org_id, period_start)` primary key, plus `event_count`
  and `usage_computed_at`. A pre-summed total so the dashboard's per-page-load
  read is one row rather than a sum over daily rows.

`usage_computed_at` is both the freshness stamp and the row's modification time,
which is why `usage_periods` has no `update_time` twin. It is also the load-bearing
signal for invariant 2: **a missing row means "never metered", a present row with
`event_count = 0` means "metered, and it really is zero"**. Collapsing those two
into a bare integer is the one modelling mistake this schema is shaped to prevent.

## 3.1 Timezone

**Usage is UTC end to end** — day boundaries, month boundaries, everything. There
are no per-org anniversary periods, so the window is derived from the clock
(`CalendarMonth`) and the database only supplies the org ids.

This deliberately differs from insights, which bucket in the **project's**
`reporting_timezone` (`bucketExpr` wraps the column in `toTimeZone`). An org can
own several projects in different zones, so there is no single org-level zone to
bucket by — and a usage number is an accounting figure, not a viewer's local
calendar. The consequence to be aware of: for a project on a non-UTC reporting
timezone, its insights daily buckets and the usage daily series will disagree at
the edges. They are answering different questions.

The meter's `toDate(occur_time, 'UTC')` names the zone explicitly.
`events.occur_time` is `DateTime64(3)` with no declared timezone, so the column
inherits the ClickHouse **server's** timezone at `CREATE TABLE` time — a bare
`toDate()` would silently cut days on local midnight for anyone running ClickHouse
with `TZ` set. No test covers that case: the shared test container is UTC, where
bare and explicit `toDate` are identical.

Note the same implicit assumption exists in `dashboard_event_rollup_daily`
(migration 006 uses a bare `toDate(occur_time)`), which is pre-existing and
untouched here.

## 4. Cadence

`pug cron usage` (image `cron-usage`) meters once and exits. There is no
long-running meter process — scheduling belongs to the deployment, so the cadence
is a k8s CronJob's `schedule`, not a constant in this repo. Hourly is the intended
setting, but nothing enforces it — which is why no surface may state a staleness
bound and every one must date-stamp the number it shows from `usage_computed_at`.

**A failed pass must exit non-zero.** The exit code is the CronJob's only success
signal, so `Run` propagates rather than swallows; a meter that logged and returned
nil would report green forever. Lock contention is *not* a failure: whoever holds
the lock is doing the work, and exiting non-zero there would alert on healthy
overlap. A lock stuck for good surfaces as a stale `usage_computed_at`.

Each pass runs inside `cron.WithLock`, which holds a **transaction-scoped** advisory
lock (`pg_try_advisory_xact_lock`) so a slow run overlapping the next schedule
cannot double-meter. The `xact` variant matters twice over: a session-scoped lock
taken through a pooled connection can be released on a different session or never,
and a transaction-scoped one is released as soon as Postgres notices a killed pod's
connection is gone — so a hung pass cannot wedge every later firing. Bound a hang
with the CronJob's `activeDeadlineSeconds`; pug does not impose its own timeout.

Under that lock the pass runs two jobs:

- **meter** — re-counts a trailing window (`PUG_USAGE_RESCAN_DAYS`, default 2)
  to absorb late arrivals, upserts the day cells, drops any cell in the window
  ClickHouse no longer returns, then re-sums every org's current period. Once
  every 24h it widens the window to the whole current month, catching late
  arrivals that fell outside the trailing rescan.

  The drop half matters because counts have to converge **downward** too: GDPR
  erasure hard-deletes events (`ALTER TABLE events DELETE`), and an upsert-only
  pass writes just the keys ClickHouse returned, so a day emptied by an erasure
  would keep its old count forever. A window that comes back with **no cells at
  all** skips the drop and logs a warning instead — that is far likelier a
  misconfigured ClickHouse than a genuinely idle deployment, and reconciling on
  it would wipe the window.
- **prune** — daily, drops `usage_daily` rows older than ~13 months. Daily rather
  than hourly because no index leads with `day`, so the delete scans the table and
  the retention boundary only moves once a day.

"Once a day" for both is tracked in **`cron_state`** (`internal/app/cron`), shared
by every scheduled job. It cannot live in the process: each pass is a fresh one, so
in-memory timestamps would make *every* run do a full-month recompute and a
full-table prune. The table is what lets the schedule fire as often as it likes
without the daily work following it.

`cron.State` binds the job name at construction and prefixes it onto every key
(`usage.prune`), so two jobs choosing the same task name cannot share a row. The
advisory-lock key comes from `cron.LockKey` — iota, not hand-picked, because a
collision is silent and reads as success: the second job finds the lock held,
concludes another pass is covering its work, and exits 0 having done nothing.

`cron_state` is deliberately **control state, not run history**. It is written only
on success and read only to gate work. "Did the job run, and did it work?" is not
its question — see §7.

Day cells are upserted in pipelined batches of 1000: the meter writes one row per
project per day, so a serial loop would be thousands of round-trips per pass. The
upsert resolves `project_id → org_id` **in SQL**, so a project deleted between the
ClickHouse read and the Postgres write inserts nothing — its events belong to
nobody.

Two conflict branches, gated differently on purpose:

| Statement | Gate | Why |
|---|---|---|
| `UpsertUsageDaily` | `where event_count is distinct from excluded.event_count` | A finished day inside the rescan window would otherwise be rewritten every pass — dead tuples, WAL and index churn for an unchanged number. |
| `RefreshUsagePeriod` | ungated | `usage_computed_at` must advance every pass, idle org or not. One dead tuple per org per pass is that stamp's price. |

The work list comes from `orgs`, not from `usage_daily`, so an org that has never
sent an event still gets a period row — and therefore reads as a metered zero
rather than as an unmetered unknown.

## 5. RPC surface & authorization

One RPC: `dashboard.usage.v1.UsageService/GetUsage` (JWT, org-scoped, **no**
`x-project-id` — usage spans every project the org owns).

It returns the current period's total, the period bounds, `usage_computed_at`, and
the per-project daily series. The `range` field bounds the **series only**; the
headline total always covers the current period. A range is capped at 400 days by
a protovalidate CEL rule, and retention prunes past ~13 months anyway.

`used_events` and `usage_computed_at` are set **together or not at all**, so a
client cannot read a count the server has no basis for — invariant 2 is enforced
by the wire shape rather than by client discipline. One wrinkle the meter creates:
at a month rollover the new period has no `usage_periods` row until the next pass,
which is *not* "never metered". `GetPeriodUsage` therefore falls back to the org's
most recent stamp, so the 1st of the month reads as a fresh zero rather than
flipping every dashboard to "unknown" for a whole schedule interval.

Authorization is `authz.ResourceUsage` + `ActionRead`, recorded in
`authz_registry.go` and enforced by `rpc.AuthzInterceptor` before the handler
runs. It sits on the **viewer floor**: the person who notices a spike is rarely
the admin. There are no create/update/delete actions — the meter writes these
tables, no RPC does.

Because the interceptor has already resolved the caller's role in the org, a
request that reaches the handler is proof the org exists and the caller belongs to
it. The handler does no existence check of its own.

## 6. Configuration

| Var | Default | Meaning |
|---|---|---|
| `PUG_USAGE_RESCAN_DAYS` | `2` | Trailing window the meter recomputes each pass. |

That is the whole surface. There is no enable flag: scheduling `pug cron usage` is
the switch, and the RPC serves whatever it stored.

Nothing runs the meter implicitly — not `pug server`, not `pug dev`. A deployment
(or a developer) that wants numbers schedules the job; until then `GetUsage`
answers with an absent `usage_computed_at`, never a fabricated zero.

## 7. Knowing whether the meter ran

Deliberately not a table in this database. A job can only record failures it
survives long enough to write, and the failure modes that actually bite — OOM kill,
image pull failure, node eviction, a wrong `schedule`, the CronJob never being
applied at all — are exactly the ones where nothing gets written. A run-history
table is blind precisely where you need it, and it disagrees with k8s the moment a
pod dies mid-pass (the table says "running", the Job says failed).

Three layers that do work, in order of usefulness:

1. **`usage_periods.usage_computed_at` going stale.** An outcome check, not a
   process check, so it catches every failure mode including the ones above and
   "nobody ever scheduled it". This is the alert worth having.
2. **The CronJob's own Job objects** — start/completion times and exit codes, which
   is why `Run` propagates the error rather than swallowing it.
3. **OTLP** — the pass logs and `telemetry.RecordError`s at source.

## 8. Known imprecision

- **Client clock skew** can place an event's `occur_time` in a neighbouring month;
  ingestion does not clamp it. A skewed client shifts a small number of events
  between periods.
- **A closed month is never revisited.** At its widest (the 24h full recompute)
  the metered window's floor is the 1st of the current month, or
  `now - PUG_USAGE_RESCAN_DAYS` if that is earlier — so it reaches into the
  previous month only for the first `PUG_USAGE_RESCAN_DAYS` of a new one. An
  import carrying months-old `occur_time` values therefore lands in **neither**
  the daily series nor any headline total: `MeterWindow` never sees those days,
  so no day cell is written and no closed period is re-summed. Deletions in a
  closed month are unrepairable for the same reason — the drop pass only
  reconciles the window it just metered. Widening `PUG_USAGE_RESCAN_DAYS` is not
  the fix: it recomputes older day cells while `OrgPeriods` still re-sums only
  the current month, which leaves the daily series and the headline disagreeing
  about a closed period.
- **`uniqExact(event_id)` per day is looser than the storage dedup key** (which
  also carries minute and kind): an `event_id` counts once per `(project, day)`
  however many kinds it arrived under, and once again in each other day it
  appears in.
- **An all-empty window is never reconciled** (§4), so a window whose events
  really were all erased keeps its stale day cells, and the period stays
  over-counted until a later window comes back with cells. That is the cost of
  not letting one suspicious ClickHouse read wipe stored days — a wipe the next
  pass repairs only for days still inside the widest window.
- **Usage is as stale as the schedule**, and every surface that shows it must say
  so from `usage_computed_at` rather than assuming a cadence.

## 9. Out of scope

- **Plans, quotas, limits and enforcement.** There is no tier model here — see
  invariant 3.
- **Payments of any kind.** No provider, no checkout, no ledger, no webhooks.
- **Per-period usage history in the API.** `GetUsage` answers for the current
  period; older totals live in `usage_periods` but are not served.
- **Usage on the SDK/API-key boundary.** Read surface is the dashboard only.
