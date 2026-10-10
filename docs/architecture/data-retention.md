# Data retention

> **Status: phases 1 and 2 implemented** (2026-10-10): every org resolves a
> length, and `pug cron retention` tracks it and logs the deletes it would make.
> Nothing deletes until phase 3 turns on `PUG_RETENTION_ENABLED`.

## Summary

Every org has a retention: how long its event history is kept. Once a day, a
new job, `pug cron retention`, deletes history older than that, a whole month at
a time.

Today nothing deletes: the job ships switched off and only logs. Every org
resolves a length, which `GetBillingStatus` and `pug billing show` report.

Free orgs keep 1 year and paying orgs 5 years. Production runs with billing off,
so nothing goes there until billing is turned on, unless an org is given its
own length. Events that a wrong device clock dated years back go at an org's
first cut, since `occur_time` has no lower bound. The dry run shows them first.

## The length

An org's length is the first of these that is set:

1. `retention_days_override`, set with `pug billing set --retention-days`.
2. With billing on, 5 years for a paying org and 1 year for a free one. Both are
   placeholders.
3. `PUG_RETENTION_DAYS`, the default with billing off. Unset keeps everything.
   The server, `pug billing`, the billing crons and `pug cron retention` read
   it, so set it on each.

Paying means a live subscription, `active` or `past_due`, a deal included.
Every other status counts as free. An org that stops paying reports 1 year at
once, but its deletes keep 5 years until an operator expires it:

```bash
pug retention expire <org-id> --actor <who>
```

That starts the 30-day wait to 1 year. Paying again before it ends keeps 5
years. The command refuses an org that still pays or has nothing waiting. An
org that starts paying gets 5 years at once, but what its free year already
cut stays gone.

What phase 1 changed in billing:

1. `placeholderRetentionDays` is `5 * RetentionYearDays`, 1,825 days. It was
   a year, and changed in place because no org held the plan.
2. A free org gets `freeRetentionDays`, 1 year, also a placeholder, not the
   current plan's `RetentionDays`.
3. The override applies on its own: billing on or off, with or without a deal.
   `set --plan free` without `--until` keeps it. Only `--retention-days 0` and
   `pug billing clear` remove it. It outlives a deal, so the operator clears it
   when one ends.
4. `GetBillingStatus.retention_days` and `pug billing show` report this length.

`pug billing set` runs with billing off too, so self-hosted operators use it.
`--plan` is required: pass the org's current `free` or `custom`. On a comp, pass
its `--until` too, or `free` ends the comp.

## What gets deleted

Only ClickHouse event history.

| Table | A row goes when |
|---|---|
| `events` | its `occur_time` is before the cut |
| `dashboard_event_rollup_daily` | its `day` is before the cut |
| `property_keys_event_buckets` | its `bucket_time` is before the cut |
| `dashboard_session_rollup` | the session ended before the cut |
| `distinct_id_activity_states` | the person's last event is before the cut |
| `event_names` | the name was last seen before the cut |

The last three hold merged states. Each data part keeps its own partial row per
key, and a delete checks rows one at a time. So these three are cut by key:
`(project_id, <id>) IN (select … group by project_id, <id> having maxMerge(<state>) < cut)`,
with `<id>` the session, the person or the event name. The id leaves out `bot`,
and `kind` for sessions, since those split one session or person over several
rows. A session or a person on both sides of the cut stays whole, all-time
counts included. Checked on ClickHouse 26.5, where a row-level
`finalizeAggregation(<state>) < cut` dropped the older part's counts instead.

Profiles, aliases, devices, usage rows, dashboards and ledgers stay.

## How it works

### The cut

The cut is the first day of the month that holds `now − length`. It is never
later than the first day of the previous month. `now − length` uses `AddDate`:
a `time.Duration` overflows past 106,751 days, and a huge length typed to mean
forever would then cut last month. `AddDate` wraps too, near the largest length,
so the length is checked first: one reaching back before 1970 cuts nothing.

| Length | Run on | Cut |
|---|---|---|
| 7 days | 2026-10-09 | 2026-09-01 |
| 90 days | 2026-10-09 | 2026-07-01 |
| 365 days | 2027-10-09 | 2026-10-01 |
| 1,825 days | 2031-07-15 | 2026-07-01 |

**Whole months**, because `events` is partitioned by month and a delete rewrites
every part it touches. A cut that moved daily would rewrite the oldest month
every day. Data stays up to a month past its length. A length under a month
keeps up to about two months, because of the rule below. A partition holds every
org, so a cut deletes rows rather than dropping the partition.

**Never the previous month**, because usage and billing re-read only current
periods, and none starts before it. So retention never changes a count or a
bill. That holds while `PUG_USAGE_RESCAN_DAYS`, 2 by default, is at most 28. A
wider rescan re-reads cut days, and the meter drops their day counts.

### The job

`pug cron retention` runs once a day as its own CronJob, with its own advisory
lock, timeout and image, like `pug cron usage`. The logic lives in
`internal/core/retention`.

It is not part of `pug cron purge`. Project deletion runs every 5 minutes on a
3-minute budget and reads no billing. Retention runs daily and reads billing.
One pass would mix their budgets, config and exit codes, and suspending one
would stop both. The two can run at the same time. Retention skips a table with
any delete unfinished; purge skips one only while a delete names its project.

Each run:

1. Resolve each org's length, and apply the wait below.
2. Group the live projects by cut.
3. Read `system.mutations` once. Then, for each table with no delete
   unfinished, and each group with a row before its cut, queue
   `ALTER TABLE <t> DELETE WHERE project_id IN (...) AND <before the cut>` with
   `mutations_sync = 0`. `<before the cut>` is the time column or the key
   condition above, and the check for a row uses it too.

A table's deletes go in together, so they cost one rewrite of each part they
touch, as in project deletion. A statement holds at most 1,000 project ids,
which keeps it far under ClickHouse's 256 KiB query limit. A table never holds
more than one run's deletes, far below ClickHouse's limit of 1,000 unfinished.
A late event dated before a cut goes the next day. A failed run exits non-zero,
and the next day's run retries. A failing delete fails every run until it is
killed: ClickHouse retries it forever, and its table waits behind it.

A ClickHouse TTL can't do this: it can't follow a per-org length that changes,
wait, or dry-run.

### Safety

1. No length deletes nothing.
2. A shorter length waits 30 days. A longer one applies at once. This covers a
   lowered or cleared override, a typo, a lower `PUG_RETENTION_DAYS`, a shorter
   new plan, and billing being turned on. A drop because an org stopped paying
   waits for an operator's expire first.
3. `PUG_RETENTION_ENABLED` is off by default. Off, the job only logs what it
   would delete.
4. Every queued cut and every wait is logged, and so is every org waiting for an
   expire.

The wait needs one new table, `retention_state`. The job writes it, and so does
`pug retention expire`.

| Column | Meaning |
|---|---|
| `org_id` | primary key, cascades from `orgs` |
| `days` | the length enforced. Null means none. |
| `pending_days`, `pending_since` | a shorter length, and when its wait began. A null `pending_since` waits for an expire. |
| `expired_by` | the operator who ran the expire |

A longer or equal length replaces `days` at once and clears the rest. A new
shorter length starts a wait, and replaces `days` after 30 days unchanged. A
drop to the free length, for an org that had a subscription that didn't fail
and has none live, starts no wait: the job records it with a null `pending_since`, and the expire
sets that to now. An org with no row counts as none, so its first length waits
too. The table updates even with the switch
off, so turning the switch on starts no new waits.

## Cloud and self-hosted

| | Cloud | Self-hosted |
|---|---|---|
| Default | 1 year free, 5 years paying | `PUG_RETENTION_DAYS`. Unset keeps everything. |
| An org's own length | `pug billing set --retention-days` | the same |
| `pug cron retention` | a daily CronJob | the operator schedules it daily |

Replicated tables refuse the three key cuts, whose deletes hold a subquery,
unless `allow_nondeterministic_mutations` is on for pug's ClickHouse user.

## Costs

1. `events` and `property_keys_event_buckets` are partitioned by month, so a cut
   rewrites only the month it removes.
2. The other four tables are not, so each is rewritten once a month per length
   in use. On cloud that is at least two: free and paying.
3. An erasure queued behind a retention delete waits for it, as with project
   deletion.

## Phases

| Phase | Ships |
|---|---|
| 1 | The length: 1 year free, 5 years paying, the override on its own, `PUG_RETENTION_DAYS`. Nothing is deleted. |
| 2 | `retention_state`, the only migration, `pug retention expire`, and `pug cron retention` with its `cron-retention` image and CronJob, switched off. It logs would-be cuts. The CronJob reads the billing switch from the same config as the server, so the two agree on every length. |
| 3 | `PUG_RETENTION_ENABLED=true`, after reading the dry-run logs. |

Before deploying phase 1, list the stored overrides, since each starts applying
on its own:
`select org_id, retention_days_override from billing_entitlements where retention_days_override is not null`.

## Not in this design

Org admins setting their own length, a notice before a cut, showing retention in
the app, hiding data past the cut before it is deleted, partitioning the
rollups, and deleting profiles.

## Decisions for you

1. **Whole months.** Recommended.
2. **A shorter length waits 30 days, tracked in `retention_state`.**
   Recommended. It costs one small table.
3. **The current and previous months are never cut.** Recommended.
4. **`PUG_RETENTION_DAYS` unset keeps everything.** Recommended. An upgrade
   never starts deleting on its own.
5. **Profiles stay.** Recommended. They are identity, not history.
6. **No customer notice yet.** Recommended for now: production deletes nothing
   while billing is off. Turning billing on starts every free org's 30-day wait
   at once, so the pricing announcement should state the 1-year free length.
7. **The switch, off by default.** Recommended.
8. **Billing sets the default with billing on: 1 year free, 5 years paying.**
   Recommended. The alternative is `PUG_RETENTION_DAYS` everywhere: simpler, but
   free and paying can't differ.
9. **Reuse billing's override.** Recommended. The cost: it behaves unlike the
   row's other overrides.
10. **An org that stops paying keeps 5 years of history until an operator
    expires it.**
    Recommended. Nobody's paid history goes without a person deciding, and the
    30-day wait still follows as the undo for a wrong org id. The cost: an org
    nobody expires keeps 5 years.
