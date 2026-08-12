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

The scan is bounded by the query's own time window, which prunes partitions --
`events` is `PARTITION BY toYYYYMM(occur_time)`, so a trailing-window pass reads
the current month's parts and skips the rest. The meter deliberately takes **no
project filter**: one pass counts every project at once, which is what lets a
single query serve every org's period.

## 3. Storage

Two tables (migration `018_create_usage.sql`):

- **`usage_daily`** — `(project_id, day)` primary key, `org_id` denormalized,
  `event_count`. The grain is what makes one metering pass serve every org, what
  bounds `uniqExact`'s memory, and what the dashboard charts.
- **`usage_periods`** — `(org_id, period_start)` primary key, plus `event_count`,
  `period_end` and `usage_computed_at`. A pre-summed total so the dashboard's
  per-page-load read is one row rather than a sum over daily rows.

`period_end` is stored but never read back: `GetUsage` recomputes both bounds from
the clock via `CalendarMonth`, because the period it answers for is always the
current one. It is kept because the row is a record of what was summed — a period
whose end is only inferable from the reader's clock cannot be audited later, and
any non-calendar period (anniversary billing) would need it present from the
start rather than backfilled. Do not delete it as unused.

`usage_computed_at` is both the freshness stamp and the row's modification time,
which is why `usage_periods` has no `update_time` twin. It is also the load-bearing
signal for invariant 2: **a missing row means "never metered", a present row with
`event_count = 0` means "metered, and it really is zero"**. Collapsing those two
into a bare integer is the one modelling mistake this schema is shaped to prevent.
(Read at org level -- for the *current* period specifically there is a wrinkle,
covered in section 5.)

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
carries no zone of its own and every bare date function resolves against the
ClickHouse **server's** timezone -- read from its config at **server start**, not
frozen at `CREATE TABLE` time, so a later `TZ` change moves it under an existing
table — a bare
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
cannot double-meter. The `xact` variant matters because the pass runs on a pooled
connection: a session-scoped lock outlives the call that took it, and through a
pool that cuts both ways — an unlock routed to a different connection silently
fails (Postgres only lets the holding session release it), so the lock rides its
original connection back into the pool never released, while a later caller handed
that same connection can release a lock it never took. A transaction-scoped one
dies with its transaction. (Both kinds are released when a killed pod's backend
goes away -- that is not what `xact` buys.)

The lock tx then sits idle for the whole pass, which is exactly what a non-zero
`idle_in_transaction_session_timeout` kills -- several managed Postgres providers
ship one, and the lock would be dropped mid-pass while the work continued. The
lock tx therefore issues `set local idle_in_transaction_session_timeout = 0`
before taking the lock; `SET LOCAL` reverts when the connection returns to the
pool. `WithLock` still joins the rollback error into its return, as the backstop
for whatever else can drop the connection.

A hang is bounded twice: by `passTimeout` (30m) inside `Run`, and by the CronJob's
`activeDeadlineSeconds` if the deployment sets one. The in-process bound is not
redundant -- the manifest is not in this repo, and a pass that hangs while holding
the lock turns every later run into a green no-op, so the failure has to be
bounded by something pug ships.

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
  all** never reconciles -- with nothing to keep, every stored row in the window
  counts as unmetered and the drop would wipe it. That guard sits in
  `DeleteUnmeteredDays` itself, not only at its caller.

  An empty read is then classified rather than assumed. The pass counts the day
  cells already stored over the same window:

  - **none stored** -- a genuinely idle or brand-new deployment. Warn, refresh
    every org's period as normal (that is what makes an eventless org read as a
    metered zero), exit 0.
  - **some stored** -- a contradiction idleness cannot produce: ClickHouse
    returned nothing over days pug has already counted. Refreshing here would
    advance `usage_computed_at` over counts the pass never verified, and since the
    query *succeeded*, a stale stamp is the only layer that would ever catch it.
    So the pass refreshes **no** period, logs at ERROR and records the error --
    and still exits 0, because a transient bad read must not thrash the CronJob.
    The stamp goes stale and section 7's layer 1 fires on its own.

  A **non-empty** read gets its own check, because "returned cells" is not the
  same as "returned cells about this deployment". `UpsertUsageDaily` resolves
  `project_id -> org_id` in SQL, so a cell whose project this Postgres does not
  know writes nothing and reports success. A ClickHouse and a Postgres pointed at
  different environments therefore meter thousands of cells, store none, and would
  then reconcile away everything earlier correct passes stored and stamp every org
  with a zero -- all while exiting 0. Counting stored rows cannot detect it (the
  cells from those earlier passes are still there), so the pass asks directly:
  `CountKnownProjects` over the project ids it just read. **None known** takes the
  same disposition as a suspicious empty read -- reconcile nothing, refresh
  nothing, record, exit 0. **Some known** reconciles as normal but warns, since a
  deleted project produces the same shape and only the operator can tell them
  apart.

  Cells the reconcile *does* drop are recorded, not merely logged. Inside a
  healthy deployment the only cause is an erasure or a dropped partition, but a
  truncated read produces the identical shape -- ClickHouse's
  `*_overflow_mode='break'` and an unavailable shard both return partial results
  with **no error**, and the query succeeding is precisely why no other layer can
  see it.
- **prune** — daily, drops `usage_daily` rows older than ~13 months. Daily rather
  than hourly because the retention boundary only moves once a day, so a more
  frequent pass would re-scan the same range to delete nothing.

Both day-range deletes are indexed: `usage_daily_day_idx (day)` exists precisely
because neither the primary key `(project_id, day)` nor `(org_id, day)` leads with
`day`, and the reconcile delete runs on **every** pass.

"Once a day" for both is tracked in **`cron_state`** (`internal/app/cron`), shared
by every scheduled job. It cannot live in the process: each pass is a fresh one, so
in-memory timestamps would make *every* run do a full-month recompute and a
full-table prune. The table is what lets the schedule fire as often as it likes
without the daily work following it.

A job's two identities travel together as a `cron.Job`: the advisory-lock key that
makes its passes mutually exclusive, and the `cron_state` prefix its task
timestamps live under. `cron.State` prefixes that name onto every key
(`usage.prune`), so two jobs choosing the same task name cannot share a row, and
passing the `Job` rather than a key plus a bare string is what stops the two
halves disagreeing — a job taking one lock while stamping another's state.

The key itself is a hand-picked 64-bit namespace (`0x7075670000000000`) plus iota.
iota rules out collisions among pug's own jobs; the namespace offset handles
everything else, because `pg_try_advisory_xact_lock` shares one global keyspace
per database with every other tool that takes an advisory lock there, and a small
integer is what an unrelated tool is likeliest to pick. A collision is silent and
reads as success: the second job finds the lock held, concludes another pass is
covering its work, and exits 0 having done nothing.

Task names are a `cron.Task` type rather than bare strings for the same reason a
typo there is expensive and silent — `LastRun` finds no row, reports the zero time
forever, and a task meant to run once a day runs on every single pass.

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
a protovalidate CEL rule, and retention prunes past ~13 months anyway. The series
itself is capped at `coreusage.MaxDailyRows` (10,000): the 400-day cap bounds the
days, nothing bounds projects-per-org, and a row is (day × project) — so without
it a large org's wide request materializes the product. Rows come oldest-first, so
a truncated series loses its newest days rather than an arbitrary slice.

**Two fields, three states**, which is what keeps a client from ever reading a
count the server has no basis for — invariant 2 is enforced by the wire shape
rather than by client discipline:

| `usage_computed_at` | `used_events` | Meaning |
|---|---|---|
| absent | absent | The meter has never run. Render "unknown", never 0. |
| present | absent | The meter is alive but has not summed this period yet. Render "computing". |
| present | present | A real total. |

The middle state is the wrinkle the meter creates: at a month rollover the new
period has no `usage_periods` row until the next pass, which is *not* "never
metered". `GetPeriodUsage` falls back to the org's most recent stamp so the 1st of
the month does not flip every dashboard to "unknown" for a whole schedule
interval — but it leaves `Counted` false, so no count goes out beside that stamp.
An earlier revision emitted a placeholder `used_events: 0` there and asked clients
to notice that `usage_computed_at < period_start`; that was client discipline
re-entering through the back door, and the gap it papered over grows without
bound whenever the meter stops running.

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
| `PUG_USAGE_RESCAN_DAYS` | `2` | Trailing window the meter recomputes each pass. Unset, `0` and negative all fall back to 2 — the trailing rescan cannot be turned off. Above the 390-day retention window it clamps to it, since a wider scan re-inserts cells the same pass's prune deletes. |

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
   process check, so it catches the failure modes above and "nobody ever
   scheduled it". This is the alert worth having.

   It only works because nothing advances the stamp on a pass that did not verify
   a count. That is why a suspicious empty read (section 4) refreshes no period at
   all: the one failure this layer could otherwise miss is the one where the
   ClickHouse query *succeeds* and returns nothing, since every other failure
   already stops the pass before it refreshes.
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
  pass repairs only for days still inside the widest window. The same window also
  refreshes no period, so the counts freeze **visibly**: the stamp stops advancing
  rather than reporting fresh-but-frozen numbers.
- **A partially truncated read is still not *prevented*, only reported.**
  ClickHouse's `*_overflow_mode='break'` settings return partial results with no
  error, so a read that comes back short -- but not empty, and naming projects
  this Postgres knows -- passes both section 4 checks and reconciles against what
  it did return, dropping the cells it did not. What changed is that the drop is
  now `telemetry.RecordError`ed rather than only warned, so it reaches OTLP
  instead of depending on someone reading a WARN log. pug sets no client-side
  overflow settings, so the server profile governs.
- **Usage is as stale as the schedule**, and every surface that shows it must say
  so from `usage_computed_at` rather than assuming a cadence.

## 9. Out of scope

- **Plans, quotas, limits and enforcement.** There is no tier model here — see
  invariant 3.
- **Payments of any kind.** No provider, no checkout, no ledger, no webhooks.
- **Per-period usage history in the API.** `GetUsage` answers for the current
  period; older totals live in `usage_periods` but are not served.
- **Usage on the SDK/API-key boundary.** Read surface is the dashboard only.
