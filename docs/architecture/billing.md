# Billing — entitlement

Design reference for the first billing slice (`internal/core/billing`,
`proto/dashboard/billing`, `pug billing`). Linked from the root
[`CLAUDE.md`](../../CLAUDE.md) — read this when working on plans,
quotas or trials. Event **counting** is not here: see [`usage.md`](usage.md).

> **Status: implemented**, except where §14 records a divergence. The code is now
> the authority; this document explains why it is shaped the way it is. This is
> the first of three billing slices (§11) and is useful on its own, with no
> payments provider involved.

Usage metering answers *how many events did this org send*. This slice answers
the other half — *how many was it entitled to send* — and nothing else. No card,
no checkout, no invoice, no enforcement.

---

## 1. Scope

**In:** every org has an entitlement (a plan, a monthly event quota, a state);
an operator can grant, extend or clear one; the dashboard can read it. Plus one
change to shipped code: the usage meter's window becomes per-org, because the
quota runs on a billing anniversary (§6.1).

**Out, by construction:** payment providers, checkout, webhooks, invoices,
payment ledgers, plan-change flows, dunning, and any enforcement whatsoever. §11
says where each of those lands.

The product state this slice delivers is real, not a stub: trials expire, paid
and negotiated plans carry their quota, and the dashboard can render "1.2M of
5M events this month". The only missing affordance is self-serve purchase —
until §11's second slice, an upgrade is an email to us and one CLI call.

## 2. Structural invariants

Four properties everything below preserves.

1. **Ingestion never consults billing.** No event is rejected, throttled,
   delayed or dropped because of a quota. No ingestion path imports
   `internal/core/billing`, and the package reads no ClickHouse at all. A quota
   drives a banner; that is its entire job. This keeps the subsystem most likely
   to be misconfigured structurally incapable of losing customer data.
2. **Signup never writes billing.** An org with no `billing_entitlements` row is
   the *normal* state, not a defect — it resolves from `orgs.create_time`. Org
   creation therefore cannot fail on a billing table, and a database whose
   billing table is empty forever behaves identically to a fresh one.
3. **Every state is derived from data that already exists.** Trial expiry,
   contract expiry and the quota window are computed at read time from
   timestamps and the clock. There is no stored status, no state machine and no
   sweep job to keep them honest, so no background outage can make an
   entitlement wrong.
4. **Every entitlement change is recorded.** The row is what the product reads,
   but it is mutated in place, so on its own it can only ever answer *what is
   true now*. `billing_entitlement_history` (§5.1) is append-only and answers
   *what was true then, and who did it* — the question that actually gets asked
   about a commercial agreement, usually months later and usually under
   pressure. A history that starts when someone first needs it is worthless, so
   it starts now.

## 3. Locked decisions

| Decision | Choice | Why |
|---|---|---|
| Billing tenant | **Org** | Orgs already own projects, members and the admin boundary, and `usage_periods` already sums per org. One entitlement per org, quota spanning all its projects. |
| Plan catalog | **Go, not rows** | A tier is (slug, name, price, quota) — static product config with revenue consequences, so it belongs in review and deploy, not in a table an operator edits at 2am. It also means no seed step and no catalog row a signup could depend on. Revisit when provider product ids arrive (§11). |
| Repricing a tier | **Never in place — mint a new slug** (§4.2) | A Go catalog has no plan versions, so editing a sold tier's numbers changes what every existing customer on it gets, retroactively, on deploy. A commercial change disguised as a one-line edit is the most dangerous thing this design could allow. |
| Money amounts | **Always (integer minor units + ISO 4217 code)** | A price without a currency is only unambiguous while there is exactly one, and a merchant of record presents local currency by design. Adding the code later means auditing every stored amount to guess what it meant. |
| Entitlement changes | **Append-only history** (§5.1) | Invariant 4. |
| Negotiated deals | **Overrides on the org's own row** (§4.1) | A bespoke deal is name, price, quota and term for exactly one org. Nullable columns layered over a catalog plan hold that, where a private-plan catalog or a discount percentage recombined with a base price would both need a table and a join to say the same thing. |
| Entitlement state | **Derived, never stored** | A `status` column is a second source of truth that can disagree with the timestamps beside it, and keeping it honest costs a worker. Every state this slice has is a comparison against `now`. |
| Quota window | **Billing anniversary**, anchored to `orgs.create_time` | An org's month runs from the day it signed up, which is the date its trial already runs from. The alternative — a calendar month — is one line of code cheaper but resets everyone on the 1st regardless of when they bought, which is a support conversation the day a card is charged. §6.1. |
| Anchor representation | **Day-of-month integer, UTC midnight** | The meter's period sum is exact only for midnight-aligned windows. An anchor stored as an instant would silently drop a partial day from the total while leaving it in the daily series. §6.1. |
| Unpaid orgs | **14-day trial → free tier** | Trial is the org's age, not stored state: no row, no provider object, no card. |
| Quota audience | **Every org member** | Reads sit on the viewer floor, exactly like `ResourceUsage`: the person who notices the limit is rarely the admin. |
| Enforcement | **None** | Invariant 1. |
| Grant mechanism | **CLI only** | pug has no staff/superadmin concept, and inventing one to put a quota field on a web page is not worth the auth surface. `pug billing` sits at the same trust level as `pug postgres migrate`. |

## 4. The plan catalog

`internal/core/billing/plans.go` — an ordered slice of `Plan{Slug, DisplayName,
Currency, PriceCents, IncludedEvents, Retired}`, with `PlanBySlug` for lookup.

| slug | name | price | included events / month | retired |
|---|---|---|---|---|
| `free` | Free | $0 | 10,000 | no |
| `trial` | Trial | $0 | 500,000 | no |
| `starter` | Starter | $10/mo | 100,000 | no |
| `growth` | Growth | $20/mo | 500,000 | no |
| `scale` | Scale | $30/mo | 1,000,000 | no |
| `custom` | Custom | — | *set per org* (§4.1) | no |

- `free` and `trial` are the **floors**, the answer when nothing else applies.
  `trial` is never stored at all — `extend-trial` writes a `free` row plus a
  `trial_ends_at`; `free` may be granted explicitly, which is how a comped
  free-tier bump is recorded.
- **`Currency` is mandatory and travels with every amount** (ISO 4217, `USD`
  throughout today). `PriceCents` is minor units of *that* currency, which is not
  always 1/100 — JPY has no minor unit, KWD has three — so nothing may assume
  cents when formatting. Storing an amount without its code is how a price
  silently changes meaning the first time a second currency exists.
- **`Retired`** marks a tier that may no longer be granted to an org not already
  on it. A retired tier stays in the catalog forever so its existing customers
  keep resolving (§4.2), and `SetPlan` refuses it for anybody else
  (`ErrPlanRetired`). It is deliberately **not** "purchasable": `custom` is
  operator-assignable and will never appear in a purchase catalog, and that
  second distinction belongs to checkout, which does not exist yet — so nothing
  here carries it.
- `PriceCents` is **display copy** in this slice — nothing charges it. When a
  provider lands it becomes a mirror of the provider's price, and the trap that
  comes with that (editing it in pug does not reprice a live subscription) is
  §11's problem, not this slice's.
- **The marketing site's pricing page is a second copy of this table**, hand-
  maintained in a different repo. Nothing enforces that they agree; a price
  change is two PRs, and this one is the one customers are actually held to.
- **Every catalog plan has a finite quota.** "Unlimited" is not a plan — it is
  what an *absent* quota means on the wire (§7), which arises from billing being
  disabled or from an unresolvable row. If a genuinely unlimited tier is ever
  sold, it gets its own slug and its own decision then.
- Adding a tier is a Go const and nothing else. `plan_slug` carries **no** check
  constraint: a list of slugs in the migration would be a second catalog to keep
  in lockstep, and the reprice workflow above — which mints a new slug — would
  fail against the stale copy with a raw SQLSTATE. `SetPlan` rejects a slug the
  catalog does not know, which is the same guard one layer up.

### 4.1 Negotiated deals

A deal we agree with one customer — "Acme, 5M events, $400/mo, annual, invoiced
by wire" — is **not** a catalog entry. It is the org's own row, carrying up to
three overrides that layer over whichever plan it names:

| Field | Column | NULL means |
|---|---|---|
| Quota | `included_events_override` | use the plan's number |
| Display name | `display_name_override` | use the plan's name |
| Price shown | `price_cents_override` + `currency_override` | use the plan's price and currency |

Term is `contract_ends_at`, and the paperwork lives in `note`. Nothing about a
bespoke deal needs a deploy, a catalog row or a join.

The two price columns **travel together** — a check constraint rejects a currency
without an amount — because a deal denominated in another currency is exactly the
case that makes a bare `price_cents` wrong, and it is the enterprise case, not an
exotic one.

The `custom` slug exists for a deal that is not a variation on a tier: it has no
quota of its own, so **`plan_slug = 'custom'` requires
`included_events_override`**, enforced by a check constraint rather than by the
CLI remembering to ask. Its price is whatever `price_cents_override` says, and
NULL there means the dashboard shows no price at all — which is usually right for
a contract nobody buys from a page. `custom` is never purchasable and never
appears in a future `ListPlans`.

**Where this stops being the right shape:** overrides describe *one* org. Sell
the same bespoke terms to twenty customers and there are twenty rows to keep in
step — at which point it has stopped being a deal and become a tier, and belongs
in the catalog (§4) as a const, or in §11.2's table once one exists.

**A plan entitles a quota and nothing else.** There are no plan-gated features in
pug, so a deal cannot grant one. If features ever become plan-scoped, that is a
new decision, not an override column.

### 4.2 Repricing, and why tiers are immutable

A Go catalog has no plan versions. Editing `growth` from 500,000 to 300,000
changes what **every existing growth customer** gets, retroactively, the moment
the deploy lands — a renegotiation of every live agreement performed by a
one-line diff, with no record that it happened and nothing to compare against.
This is the one failure mode that a rows-based catalog handles for free (the
archived design's `PLAN_STATUS_ARCHIVED` existed for exactly this), so a Go
catalog has to buy it back with a rule:

> **A tier's `PriceCents`, `Currency` and `IncludedEvents` are immutable once any
> org holds it.** Repricing mints a new slug — `growth-v2` — and marks the old
> one `Retired: true`. Nothing is ever deleted from the catalog.

Existing customers keep resolving against the slug they hold and are unaffected;
new ones get the new tier. Grandfathering is then the default rather than
something an operator has to remember, and moving a customer onto new terms
becomes what it should be — a deliberate `pug billing set`, recorded in the
history (§5.1) with a note about who agreed to it.

What stays editable: `DisplayName`, because renaming "Growth" to "Team" changes
nothing anyone bought. What this costs: a catalog that only grows, and slugs
that carry a version suffix. Both are cheap next to a silent quota cut.

## 5. Storage

Migration `019_create_billing_entitlements.sql`: the entitlement itself, and the
history behind it (§5.1). The entitlement is a 1:1 extension of `orgs`, so the
org id is the primary key rather than a `char(20)` xid of its own — one row per
org is then structural rather than a constraint somebody has to remember to add.

```sql
create table billing_entitlements (
  -- NULL means the anchor is orgs.create_time's day of month, which is the case
  -- for every org until §11.2 has a charge date to align to.
  anchor_day smallint
    constraint billing_entitlements_anchor_day_check
      check (anchor_day is null or anchor_day between 1 and 31),
  contract_ends_at timestamptz,
  create_time timestamptz not null default now(),
  -- varchar, not char(3): char blank-pads a short code into the value a client
  -- formats against, and this column is the only way a non-USD amount enters pug.
  currency_override varchar(3)
    constraint billing_entitlements_currency_check
      check (currency_override is null or currency_override ~ '^[A-Z]{3}$'),
  display_name_override varchar(150),
  included_events_override bigint
    constraint billing_entitlements_override_check
      check (included_events_override is null or included_events_override > 0),
  note text not null default '',
  org_id char(20) primary key references orgs(id) on delete cascade,
  -- No slug check: the catalog is Go (plans.go) and SetPlan already rejects an
  -- unknown slug. A list here would be a second catalog to migrate in lockstep.
  plan_slug varchar(50) not null,
  price_cents_override bigint
    constraint billing_entitlements_price_override_check
      check (price_cents_override is null or price_cents_override >= 0),
  trial_ends_at timestamptz,
  update_time timestamptz not null default now(),
  -- A custom plan has no catalog quota to fall back on, so the deal is
  -- unrepresentable without this. Enforced here rather than in the CLI: the row
  -- is what every read trusts.
  constraint billing_entitlements_custom_needs_quota
    check (plan_slug <> 'custom' or included_events_override is not null),
  -- An amount is meaningless without its unit, so the currency can only exist
  -- alongside one.
  constraint billing_entitlements_currency_needs_price
    check (currency_override is null or price_cents_override is not null)
);
```

- **`anchor_day`** — the day of month the org's quota window starts on. NULL
  means `orgs.create_time`'s day, which is every org today (§6.1). It is a
  day-of-month integer rather than a date or an instant so that a period can only
  ever start at UTC midnight, which is what the meter's sum requires.
- **`contract_ends_at`** — when a granted plan lapses back to the floor. NULL
  means open-ended. It is the end of the *deal*, deliberately not the end of a
  quota window — an annual contract ending in March does not make March's quota
  window a year long. Keeping the two apart is why `anchor_day` exists as its own
  column rather than being read off whichever date happens to be nearby.
- **The `*_override` columns** — a negotiated deal's quota, name, price and
  currency (§4.1). NULL means "use the plan's" in each case.
  `included_events_override` is checked `> 0` because 0 would read as a quota of
  zero rather than as "no override", and because "unlimited" is deliberately not
  expressible here; `price_cents_override` allows 0, since a comped account is a
  real deal.
- **`trial_ends_at`** — set only by `extend-trial`. NULL means the trial window
  is derived from `orgs.create_time`, which is the ordinary case for every org.
- **`note`** — the operator's record of why ("annual wire, INV-123"). Never
  returned by any RPC; it is for `pug billing show`, which prints it on the
  stored row, and for `show --history`.
- **No `status`, no `provider`, no `id`.** The first is derived (§6), the second
  has nothing to distinguish yet, the third has no use when `org_id` is unique.

The migration seeds nothing and backfills nothing. Every org that exists today
gets its correct entitlement from invariant 2 the moment the code deploys.

### 5.1 History

Every write to `billing_entitlements` appends a full snapshot of the row as it
now stands, in the same transaction:

```sql
create table billing_entitlement_history (
  actor varchar(150) not null,
  changed_at timestamptz not null default now(),
  id char(20) primary key,
  -- The entitlement as of this change, verbatim. NULL across the value columns
  -- is a deletion: the org returned to the derived floors.
  anchor_day smallint,
  contract_ends_at timestamptz,
  currency_override varchar(3),
  display_name_override varchar(150),
  included_events_override bigint,
  note text not null default '',
  -- No FK: the history outlives the row, and an org's terms are still the
  -- answer to a question after the org is gone.
  org_id char(20) not null,
  plan_slug varchar(50),
  price_cents_override bigint,
  trial_ends_at timestamptz
);

create index billing_entitlement_history_org_idx
  on billing_entitlement_history (org_id, changed_at desc);
```

- **Snapshots, not diffs.** One row is the complete answer to "what were Acme's
  terms in March", with no replay and no dependence on every earlier row being
  intact. Diffs are smaller and are the wrong trade at a handful of rows per
  customer per year.
- **`actor` is required.** Every mutating command takes `--actor` and cobra
  refuses the command without it; §11.2's webhook writes will record the
  provider. It is stated rather than detected because these commands run from a
  pod, where the OS user is the image's uid and reads the same for every
  operator. An unattributed change to a commercial agreement is barely better
  than no record, so there is no default and no nullable column to leave empty.
- **No foreign key to `orgs`.** `billing_entitlements` cascades away with the
  org; the history must not, because "what were they on when they left" is
  precisely a question asked after deletion — in a refund dispute, most often.
- **Append-only by convention, and nothing in the codebase updates or deletes
  it.** There is no RPC that reads it either: it is operator and support data,
  reached through `pug billing show --history`.

## 6. Resolution

`billing.Resolve(orgCreateTime, row, now, billingEnabled)` is a pure function
returning the resolved `Entitlement` — slug, display name, currency, status,
`PriceCents`, `IncludedEvents`, trial/contract dates and the period bounds. No
I/O, so the whole rule set is unit-testable without a container. `orgCreateTime`
is an argument rather than something the package looks up because it is
load-bearing twice: it is the trial clock, and it is the default quota anchor
(§6.1).

In order:

1. **Billing disabled** (§9) → status `FREE`, plan `free`, `IncludedEvents` nil.
   A self-hosted install has no quota at all, so no banner can fire even if a
   client forgets to check the flag. The switch fails *open* on the number
   because the number enforces nothing.
2. **No row** → trialing until `orgCreateTime + 14d`, free after. Derived, never
   materialized: a read must not write a row.
3. **A non-floor `plan_slug`, and `contract_ends_at` is NULL or in the future** →
   `ACTIVE` on that plan.
4. **`trial_ends_at` in the future** → `TRIALING` on the `trial` plan, using the
   stored date.
5. **Otherwise** → `FREE`.

**A granted plan outranks a live trial date** (3 before 4), so a customer who
converted mid-trial can never be demoted by a stale timestamp. `SetPlan` also
clears `trial_ends_at` when it grants a non-floor tier, so in practice the two
rarely coexist — the ordering is what makes the resolver's answer independent of
whether that write happened.

Then each present override replaces the corresponding field of the resolved plan
(§4.1) — quota, display name, price, currency — patching whatever steps 3–5
produced, so a deal survives a catalog reprice untouched.

The overrides apply **only while the granted plan the row names is still the one
resolved.** They describe a deal on *that* tier, so a lapsed contract or an
expired trial falls to the free floor with the floor's numbers rather than
keeping the negotiated ones. Without that gate an expired 5M deal would keep its
5M forever, which is the one way this function could cost real money. The one
exception: a **floor** row's overrides survive a resolved slug that differs, so a
comped free-tier bump is not wiped by the org still being in its trial (§10).

An **unknown `plan_slug`** — only reachable if a slug is removed from Go while
rows still point at it — resolves to `IncludedEvents` nil (no quota). `Resolve`
stays pure, so the log happens in `Reader.GetEntitlement`, which has a ctx to
attach it to; it is a `WarnContext` and deliberately **not** a
`telemetry.RecordError`, which would record an exception on every dashboard load
for as long as the drift lasts. Failing to "free, 10,000" would tell a paying
customer they are over their limit; failing to "no quota" is a silent banner and
a log. Nothing makes this unreachable — `plan_slug` carries no check constraint
(§5) — so only `SetPlan`'s catalog check guards it, and that cannot guard a slug
removed after the row was written.

**Expiry is lazy, always.** A trial that ended an hour ago reads as free on the
next request, with nothing having run in between. This is the whole reason §2's
third invariant is worth holding: there is no job whose failure can leave an
entitlement stale, because there is no job.

### 6.1 The quota window is a billing anniversary

**An org's month runs from its anchor day, not from the 1st.** An org that
signed up on the 17th has periods `17 Jan → 17 Feb → 17 Mar`, and that is the
window both halves of "X of Y" are measured over.

#### Where the anchor comes from

`orgs.create_time`. Every org has one, it has been NOT NULL since migration 001,
and the trial already runs from it — an anniversary anchored anywhere else would
make the entitlement hang off two different dates. So the default anchor needs no
column, no operator action and no backfill: an org that has never touched billing
still has a well-defined window, which keeps invariant 2 intact.

`billing_entitlements.anchor_day` (§5) overrides it, and is NULL for almost
every org. It exists now because §11.2 has to choose between two ways of aligning
a real charge date, and a nullable column defers that choice at no cost:

- **align the provider to us** — set the subscription's billing cycle to the
  org's existing anchor, so the invoice date and the quota reset are the same day
  forever, and `anchor_day` stays NULL. This is the better system and the
  intended one.
- **align us to the provider** — write the charge day into `anchor_day` at
  checkout. Simpler, but it moves the anchor mid-life, which truncates one period
  and gives that month a short window.

Which one ships depends on whether the provider can set a billing cycle day at
all; the column means that answer does not have to be known today.

#### The arithmetic

`usage.PeriodFor(now, anchorDay) (start, end time.Time)` replaces
`usage.CalendarMonth(now)`. **Periods always start at UTC midnight** — the
anchor is a day-of-month integer, never an instant — because
`RefreshPeriodUsage` sums `usage_daily` with `CeilDayUTC` on both bounds and is
exact only for midnight-aligned windows. An anchor stored as a timestamp would
silently drop the first partial day from the total while `ListDailyUsage` kept it
in the series, and the two would disagree with nothing to notice.

**Short months clamp, they do not overflow.** Anchor 31 lands on 28 (or 29) in
February, and Go's `time.AddDate` *normalizes* rather than clamps — `Feb 31`
becomes `Mar 3` — so this needs an explicit helper, never `AddDate` on a day
number. Each period start is re-derived from the anchor rather than from the
previous start, so a clamp does not stick: anchor 31 gives
`31 Jan → 28 Feb → 31 Mar → 30 Apr`, which stays contiguous and non-overlapping,
with every instant in exactly one half-open period. This is the classic source of
billing bugs and gets a table-driven test before anything else is written.

#### What this changed in `usage`

This is the part of the slice that touched shipped code. The storage needed
nothing: `usage_periods` is keyed `(org_id, period_start)` with no assumption that
a start is a month boundary, and `RefreshUsagePeriod` already took explicit
`period_start` / `period_end` / `from_day` / `to_day`. `usage.CalendarMonth` is
gone, replaced by `PeriodFor(now, anchorDay)`.

- **`usage.OrgPeriods`** — the meter's work list, formerly one window for every
  org — is per-org. `ListOrgUsageWindows` returns `orgs.create_time` with a
  `left join billing_entitlements` for the anchor override. SQL-level coupling
  only: `usage` imports nothing from `billing`.
- **The cron's full-recompute widening** (`meterFrom` in
  `internal/app/cron/usage/usage.go`) took the calendar-month start, which stopped
  being a lower bound on its own: an org anchored on the 17th and metered on the
  10th has a period that began *before* this calendar month. It now takes the
  earlier of the month start and `EarliestPeriodStart` over the work list — free,
  because the job already holds every window in memory at that point. The month
  floor is **kept**, not replaced: an org anchored a day or two ago has a period
  start later than the trailing rescan, so the anniversary alone would leave a
  full pass no wider than the 2-day incremental one. `PeriodFor` never returns a
  start more than ~31 days back, so the daily full pass scans the same order of
  days as before — roughly double on average (~15d → ~30d), which can touch one
  extra monthly ClickHouse partition.
- **`usage.Reader.GetOrgPeriod`** is the single derivation both `GetUsage`'s
  handler and the meter call. Two independent derivations of the same window is
  how they drift; `TestQuotaWindowMatchesTheMeters` pins that they agree across a
  full year for a derived, a mid-month and a clamping anchor.
- **Rollovers spread out.** Every org's period used to turn over at once on the
  1st; they now turn over on ~28 different days, so the "metered but this period
  not reached yet" state (`periodNotReached`) is a daily occurrence for some org
  rather than a monthly spike for all of them. The existing fallback needed no
  change — but it is now load-bearing every day, not once a month.
- **Test fixtures backdate their orgs.** `testutil.SetOrgCreateTime` exists
  because an org created "now" anchors on whatever day of the month the suite
  runs, which would make any test asserting fixed period bounds pass or fail by
  the calendar.

#### The cost, stated plainly

`usage` and `billing` shared nothing but a clock. An anniversary makes the
meter's window depend on billing data, so a wrong anchor produces a wrong total
for one org, silently, and no test inside `usage` would catch it. The mitigation
is that the default anchor is `orgs.create_time` — data the meter can read
without billing existing at all — so the failure mode requires someone to have
explicitly written an `anchor_day`.

**Rollout on a live database.** The first pass after deploy writes new
`usage_periods` rows at anniversary starts; the existing calendar-month rows stay
as historical records, and `GetUsagePeriod` no longer matches them because it
looks up by exact `period_start`. They are still reachable through
`GetLatestUsageComputedAt`, which is why that query orders by
`usage_computed_at` rather than by `period_start` — a stranded month row can have
a *later* start than the anniversary row the meter is keeping current, and
ordering by start would hand back its frozen stamp at every rollover. The same
applies after an `--anchor-day` change. Between the deploy and that pass, an org whose anniversary
row does not exist yet reads as "computing", not as zero —
`periodNotReached` already covers exactly this. No backfill is required, because
`usage_daily` keeps day grain and re-sums any window inside the 400-day
retention.

**Migration 019 must be applied before the server and `cron-usage` images roll.**
This is the one hard ordering constraint in the slice, and it is stricter than a
new table normally implies: `ListOrgUsageWindows` and `GetOrgUsageWindow` are the
*already-live* `GetUsage` read path, and both `left join billing_entitlements`.
Against a database without 019 every `GetUsage` returns `Internal` for every org
and every metering pass exits non-zero — billing being switched off does not
help, because the join is in SQL, below the flag. The same applies in reverse to
a 019 down-migration, which would take metering down rather than just billing.
Deploy ordering lives in the `pug-sh/gitops` repo; nothing in this repo enforces
it.

## 7. RPC surface & authorization

`proto/dashboard/billing/v1/billing.proto` — `BillingService`, JWT boundary,
one RPC. `org_id` is on the request so `authzspec.OrgFromMessage` resolves the
org through the generated `GetOrgId()`.

| RPC | Spec | Returns |
|---|---|---|
| `GetBillingStatus` | `OrgGated(ResourceBilling, ActionRead)` | `billing_enabled`, plan (slug, display name, price cents, currency), derived status, `included_events`, `trial_ends_at`, `contract_ends_at`, `period_start`, `period_end` |

The plan fields are the **resolved** ones — overrides already applied (§4.1), so
a client never reconstructs a deal from a base plan plus patches. `note` and the
history never cross the wire; both are operator data.

`currency` is always present alongside `price_cents`, and a client must format
from the pair rather than assuming two decimal places (§4).

- **No consumption number.** The client makes two calls —
  `UsageService.GetUsage` for X, `GetBillingStatus` for Y. Folding usage into
  this response would make billing depend on the usage subsystem and would have
  to restate its three-state freshness contract (absent / computing / really
  zero), which is exactly the kind of duplicate that drifts.
- **`included_events` is absent-able**, and absent means *no quota* — never zero.
  It is a `google.protobuf.Int64Value` wrapper, not a bare edition-2023 scalar:
  protoc-gen-go would give the scalar presence, but protoc-gen-es renders it as a
  NON-optional bigint, so absence would reach the dashboard as `0`. This is the
  one place billing diverges from `GetUsageResponse.used_events`, which is a bare
  `int64` a client can pair with `usage_computed_at` to detect absence — quota has
  no such companion field. The same applies to `Plan.price_cents`, where absent
  means "no price recorded" and 0 means comped.
  A client consuming these from `../app` must check presence rather than
  truthiness — `0` is a real value for both.
- **No `ListPlans`.** A price list whose buy button does not exist yet is a
  dialog that can only disappoint. It arrives with checkout (§11).
- **Read-only, so read-only permissions.** `authz.ResourceBilling` is added to
  the const block **and** `allResources` (`policy_test.go` fails a
  declared-but-ungranted resource), with `grant(roleViewer, ResourceBilling,
  ActionRead)` putting it on the viewer floor for member and admin to inherit.
  No create/update/delete action is granted, because no RPC performs one.

Two of the three wiring points fail a contract test if missed: the entry in
`authz_served.go` and the procedure entry in `authz_registry.go`. The third —
`handle(...)` in `server.go` — is not enforced by anything: nothing enumerates
the mux's routes, so omitting it 404s silently.

## 8. Operator CLI

```
pug billing show <org-id> [--history]
pug billing set  <org-id> --plan <slug> --actor <who> [--events N]
                          [--name "Acme Enterprise"]
                          [--price 40000 --currency USD] [--anchor-day 17]
                          [--until 2027-01-01] [--note "annual wire, INV-123"]
pug billing extend-trial <org-id> --days 30 --actor <who>
pug billing clear <org-id> --actor <who>
```

Postgres only — no provider, no network. `set` upserts the row, `clear` deletes
it (returning the org to derived trial-then-free), and `show` prints the resolved
entitlement *and* the stored row beneath it, since the interesting bugs live in
the gap between them — a lapsed deal's quota is invisible in the resolved answer
but still carries onto the next `set`.

Three boundary rules the flags do not spell out:

- **`--until` is inclusive of the date given.** The resolver's comparison is
  half-open, so `ContractEndExclusive` — beside that comparison, not in the CLI —
  stores the *following* midnight: `--until 2026-12-31` means the plan runs
  through all of 31 December. `show` prints the stored instant, which is
  therefore the 1st.
- **`extend-trial` never shortens.** It sets an absolute `now + days`, so a small
  `--days` against a trial with longer to run is refused (`ErrTrialNotExtended`)
  rather than silently cutting it. Capped at `MaxTrialDays`.
- **A `set` does not cancel a running trial.** The trial is the org's age (§6),
  derived identically whether or not a row exists, so recording an anchor day or
  a note on a three-day-old org leaves it trialing.

A negotiated deal (§4.1) is one `set`:

```
pug billing set o_2f9k --plan custom --events 5000000 --name "Acme Enterprise" \
                       --price 40000 --currency USD --actor "praveen/INV-123" \
                       --until 2027-01-01 --note "annual wire, INV-123"
```

`--events`, `--name`, `--price`/`--currency` and `--anchor-day` write the
override columns; omitting one on a re-`set` leaves the stored value alone, and
passing the empty value (`--events 0`, `--name ""`, `--price -1`,
`--anchor-day 0`) clears it back to the plan's. Leaving them alone is the right
default because the common re-`set` is a renewal — a new `--until` on terms that
have not changed — and a flag that silently reverted a customer's negotiated
quota to a catalog number would be the most expensive bug this CLI could have.

Guards on `set`, all refusing rather than guessing: an unknown slug; the `trial`
slug (`extend-trial` is its only writer); a **retired** tier (§4) unless the org
already holds it, so it cannot be handed to someone new by autocomplete; `custom`
without an `--events` quota; `--currency` without a price, and a price without a
currency — clearing `--price` clears the currency with it, since the two are one
stored pair. `--events` refuses a negative rather than reading it as a clear, and
`--anchor-day` is range-checked in both the CLI and the service.

Every write appends to the history (§5.1) in the same transaction, attributed to
the `--actor` it was given. `show --history` prints the org's
changes newest-first, which — with `note` — is what a refund or renewal argument
is actually settled from.

This CLI is why the slice is usable rather than decorative — without a writer,
the table is dead and the RPC only ever reports the derived floors. If it should
be smaller, the honest floor is `show` + `set`; `extend-trial` and `clear` are
conveniences over the same two columns.

## 9. Configuration

| Var | Default | Meaning |
|---|---|---|
| `PUG_BILLING_ENABLED` | `false` | The single switch. Off ⇒ `billing_enabled=false` and no quota anywhere (§6.1). Set it on every pod of a billed deployment. |

It follows `PUG_DEMO_ENABLED` exactly: `envconfig` on the server, which rejects
a malformed bool outright. There is no worker and no CLI gate — `pug billing`
edits rows whether or not the server is serving them, which is what makes it
usable to prepare a deployment before the switch goes on.

## 10. Testing

`internal/core/billing` needs `func TestMain(m *testing.M) { testutil.Main(m) }`
and no `t.Parallel()` for the container-backed cases
([`CLAUDE.md`](../../CLAUDE.md) § Testing). `Resolve` itself is pure, so the rule
table is a plain unit test.

- **Resolution** — no row resolves trial-then-free off `orgs.create_time` and
  writes nothing; a trial past `trial_ends_at` resolves free with no sweep
  having run; a contract past `contract_ends_at` does the same; each override
  patches only its own field and a catalog reprice leaves a deal untouched; an
  unknown slug resolves to no quota; billing disabled resolves to no quota
  regardless of the row.
- **The floor-plan corners**, which is where a comped deal lives and where three
  bugs hid: a row's existence does not end a derived trial; a floor plan's
  overrides survive the trial promotion that renames the resolved slug to
  `trial`; and a floor plan's `contract_ends_at` still expires them, so a
  time-boxed comped pilot lapses like any other deal.
- **Window** — the period `Resolve` reports and the period the meter sums are the
  same half-open window for the same clock and anchor. This is the assertion that
  keeps the two halves of "X of Y" honest, and it is the one that matters most in
  this slice.
- **Anchor arithmetic** — its own table-driven test, written first: anchor 31
  across February (28 and 29), anchor 30 across February, an anchor that clamps
  one month and un-clamps the next (`31 Jan → 28 Feb → 31 Mar`), an instant
  exactly on a boundary landing in the later period, and consecutive periods
  being contiguous with no gap and no overlap across a full year for every
  anchor 1–31. `time.AddDate` normalizing `Feb 31` into `Mar 3` is the specific
  bug this test exists to fail on.
- **Meter parity** — `GetBillingStatus` and `GetUsage` report the same window for
  the same org (`TestQuotaWindowMatchesTheMeters`), and `OrgPeriods` gives an org
  anchored on the 17th a period that began in the previous calendar month, which
  is what `EarliestPeriodStart` widens the cron's full rescan to
  (`TestOrgPeriodsUsesEachOrgsOwnAnchor`).
- **Storage** — every slug in the Go catalog except `trial`, which is never
  stored, inserts successfully, which is the
  test that catches the catalog and the check constraint drifting apart; a
  `custom` row without `included_events_override` and a currency without an
  amount are both rejected by the database, not merely by the CLI.
- **History** — every mutating CLI path appends exactly one snapshot in the same
  transaction, `clear` included; a failed write appends nothing; the history
  survives its org being deleted. The last of those is the one a foreign key
  would quietly break, so it is a test rather than a comment.
- **Catalog immutability** (§4.2) — a golden test pins every sellable tier's
  `PriceCents`, `Currency` and `IncludedEvents`, so editing a live tier fails CI
  and the fix is to mint a new slug. This is the only guard that exists against
  a one-line quota cut, since nothing else in the system can tell an intended
  reprice from a typo.
- **Authz** — no handler-level role test exists.
  `TestPermissionRegistryCoversAllProcedures` and `policy_test.go` fail until the
  registry and policy entries exist, so those are the guard rather than tests to
  write.

## 11. What comes after this

Each is its own slice, in this order, and each is additive — no slice below
rewrites what this one stores.

1. **Dashboard surfaces** (`../app`): the sidebar meter, the ≥90% banner and a
   settings section, joining `GetUsage` and `GetBillingStatus` in one shared
   atom. This is the slice that makes the entitlement visible to a customer, and
   it needs no server change. **Plus the email half** — a customer who does not
   log in never sees a banner, so the quota warning has to reach them; a banner
   alone is a notification only for people already looking.
2. **Checkout** (payments provider): products, checkout sessions, a signed
   webhook inbox and the provider-reported states this slice has no way to
   derive (`PAST_DUE`, `CANCELLED`). It brings its own tables and a `status`
   column — the entitlement row keeps meaning exactly what it means today, and
   the provider becomes one more thing that can write it. `ListPlans` and the
   plan catalog's move from Go to rows belong here too, since a purchasable tier
   is bound to a per-environment provider product id, which is the first thing
   in this subsystem that genuinely cannot be a Go const. Four things must be
   decided *in* this slice rather than discovered after it:
   - **The provider is a merchant of record** (Dodo, Paddle, Lemon Squeezy) —
     which is what makes VAT/GST registration, tax collection and legally
     compliant invoices somebody else's obligation. This is an architectural
     dependency, not a vendor preference: moving to a payments-only processor
     later makes worldwide tax pug's problem, and there is nothing in this design
     that would absorb it.
   - **Deleting an org must cancel at the provider first.** The FK cascade drops
     the entitlement row and the provider knows nothing about it, so today's
     design would leave a deleted customer being charged — a refund and a
     chargeback, not merely an inconsistency.
   - **Dunning**: what a failed renewal does to entitlement (proposed: nothing —
     `PAST_DUE` keeps the quota; degrading a paying customer's product over an
     expired card is worse than a few unbilled days), and how many notices go out
     before it lapses.
   - **Annual terms.** Standard, and it interacts with §6.1: an annual contract
     renews yearly while the quota window stays monthly, so `contract_ends_at`
     and the anchor do different jobs and both are needed.
3. **Payment ledger**: invoices, recorded manual payments, refunds and
   chargebacks, on a separate admin-only resource — amounts and invoice
   references do not belong on the viewer floor the quota banner sits on. A
   refund is a ledger row; what a *chargeback* does to entitlement is a policy
   question this slice must answer rather than inherit. A **billing contact**
   address separate from the acting admin belongs here too: finance mail should
   reach `accounts@`, not whoever last clicked upgrade.

An **archived, reviewed, all-at-once implementation** of roughly all three
exists at `archive/billing-2026-08-15` (Dodo Payments, 5 tables, ~10k lines).
It is a useful reference for §11.2's webhook and reconcile mechanics — its
delivery-semantics table in particular — but it stores what this design derives,
so its schema is not the target.

## 12. Known imprecision

- **Client clock skew** can place an event's `occur_time` in a neighbouring
  month; ingestion does not clamp it. A skewed client shifts a small number of
  events between quota periods. Accepted at these tier sizes.
- **Trial (500k) → free (10k) is a 50× cliff.** Deliberate — it is what makes
  the trial worth taking — but an org that ramps during its trial meets the
  banner the day it expires.
- **A short month shortens the period.** An org anchored on the 31st gets a
  28-day window in February against the same monthly quota (§6.1). Every
  anniversary billing system has this; the alternative — clamping the anchor
  permanently to 28 — quietly moves the reset date of every org that signed up
  late in a month.
- **Upgrading mid-period raises the quota for the whole period**, retroactively
  covering events already counted, because the quota is resolved at read time and
  is not a running balance. Generous in the customer's favour, and free while
  nothing is enforced.
- **A moved anchor truncates one period.** If §11.2 ends up aligning our anchor
  to the provider's charge day rather than the reverse, the org's current window
  is cut short once, and that month's number will look small next to its
  neighbours.
- Counting's own imprecisions — staleness, day-boundary rounding — belong to
  [`usage.md`](usage.md).

## 13. Deliberately not handled

Things a mature SaaS billing system has that this one does not. Each is a
choice, recorded so it stays one.

**Nothing bounds a runaway org, and nobody is told.** Enforcement is invariant 1
and stays that way, but the consequence is worth stating plainly: an org can send
fifty times its quota, and the only signal is a banner shown to the person with
the least reason to act on it. That is a ClickHouse cost exposure and an abuse
vector, not a billing gap — the fix belongs with the meter, which already sweeps
every project on a schedule, and is noted in [`usage.md`](usage.md). It does not
need a quota to be useful: "any org over N events/day" catches the same traffic
and works for custom deals too.

**Overage charges.** Tiers are flat. Sending more than the quota costs the
customer nothing, by decision — metered overage would need usage pushed to the
provider and a reconciliation story that neither the prices nor the volumes
justify.

**Per-seat pricing, add-ons, credits and account balances.** The product is
priced by events; members are free. Nothing here is close to needed, and each
would add a second dimension to a quota that is currently one number.

**Consolidated billing across a customer's orgs.** One entitlement per org, so a
person who admins three of them pays three times. Correct until somebody asks —
and when they do, the answer is a payer relationship between orgs, not a change
to this row.

**Discounts and promo codes.** Provider-side, if ever: a code applied at checkout
changes what is charged, not what is entitled, so nothing in this design has to
know. A permanent negotiated discount is a deal (§4.1), which is a different
thing and already handled.

**Revenue recognition, MRR reporting, deferred revenue.** The provider's
dashboard, until there is a finance function that needs otherwise.

**Anything self-serve beyond checkout.** Plan changes and cancellation go through
the provider's customer portal in §11.2; an in-dashboard switcher is a later
call.

## 14. Divergences from this design

Where the code differs from the sections above, the code wins and the reason is
here.

- **`Sellable` became `Retired`** (§4). One flag was conflating "may be granted"
  with "may be purchased", and `custom` needs the first while never having the
  second — a negotiated deal is granted to an org that has never held one, so
  a `Sellable: false` guard made every custom deal impossible to create. Splitting
  them now would have shipped a purchasability flag with no consumer, so only the
  guard's own concept exists: `Retired`. Checkout brings the other half.
- **A granted plan is resolved before a live trial date** (§6, steps 3 and 4 are
  swapped relative to the first draft). The original order let a stale
  `trial_ends_at` demote a customer who had converted mid-trial.
- **Overrides apply only while the row's plan is the plan in force** (§6). The
  first draft applied them unconditionally, which would have let an expired 5M
  deal keep its 5M for good.
- **The `GetUsage` RPC now answers `NotFound` for an unknown org.** It previously
  reported a metered zero for any id, because the period came from the clock
  alone and no lookup could fail. Resolving an anchor requires the org row, so a
  missing one is now a real error — same `ORG_NOT_FOUND` reason the orgs service
  uses.
- **`included_events` and `price_cents` ship as `Int64Value` wrappers**, not the
  bare edition-2023 scalars §7 originally specified. protoc-gen-go would have given
  those presence, but protoc-gen-es renders a singular scalar as a non-optional
  bigint, so "no quota" would have reached the dashboard as `0` — the one thing
  the field must never say. Verified against the generated TS.
- **`clear` distinguishes an unknown org from an org with no row.** It returned
  success for both, so a typo'd id printed "entitlement cleared" while the real
  org kept its deal.
- **`extend-trial` refuses an org holding a granted plan** (`ErrTrialOnGrantedPlan`).
  A granted plan resolves ahead of any trial date, so the write stored a date that
  changed nothing and still printed as a success.
- **`ListPlans` is absent as designed, but so is any RPC that reads the
  history.** §5.1 says the history is operator data; `pug billing show --history`
  is the only reader, and nothing serves it over the network.
