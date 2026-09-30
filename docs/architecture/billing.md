# Billing — entitlement

Design reference for the entitlement slice
(`internal/core/billing/entitlement`, `proto/dashboard/billing`, `pug billing`).
Linked from the root [`CLAUDE.md`](../../CLAUDE.md) — read this when working on
the usage plan, allowances or deals. Event **counting** is not here: see
[`usage.md`](usage.md).
`internal/core/billing` holds three packages: the root is the payment provider
port and its vocabulary, `entitlement` is what this document describes, and
`subscription` is the payments side, [`payments.md`](payments.md).

> **Status: implemented**, except where §14 records a divergence. **Revised
> 2026-09-27 for usage billing:** the fixed-price tiers and the trial are gone.
> The catalog is one usage plan of quantities — a free allowance and tier
> boundaries — and every rate lives on the provider's product. The code is the
> authority; this document explains why it is shaped the way it is. This is the
> first billing slice (§11); checkout is [`payments.md`](payments.md).

Usage metering answers *how many events did this org send*. This slice answers
the other half — *how much of that is free, and how the rest splits into tiers* —
and nothing else. No card, no checkout, no invoice, no enforcement, and no price.

---

## 1. Scope

**In:** every org has an entitlement (a plan, a monthly free allowance, a state);
an operator can record a comp or a deal, or clear one; the dashboard can read it.
Plus one change to shipped code: the usage meter's window is per-org, because the
allowance runs on a billing anniversary (§6.1).

**Out, by construction:** payment providers, checkout, webhooks, invoices,
payment ledgers, plan-change flows, dunning, prices, and any enforcement
whatsoever. §11 says where each of those lands; taking money is
[`payments.md`](payments.md).

What this slice delivers: every org without a subscription has the current plan's
free allowance, a subscriber is on the plan its subscription names, a negotiated
deal carries its own terms once bought, and the dashboard can render "1.2M of 5M
free events this month".

## 2. Structural invariants

Four properties everything below preserves.

1. **Ingestion never consults billing.** No event is rejected, throttled,
   delayed or dropped because of a quota. No ingestion path imports
   `internal/core/billing` or any package under it, and none of them reads
   ClickHouse at all. A quota drives a banner; that is its entire job. This
   keeps the subsystem most likely to be misconfigured structurally incapable of
   losing customer data.
2. **Signup never writes billing.** An org with no `billing_entitlements` row is
   the *normal* state, not a defect — it resolves free on the current plan, and
   its usage period anchors on `orgs.create_time` (§6.1). Org creation therefore
   cannot fail on a billing table, and a database whose billing table is empty
   forever behaves identically to a fresh one.
3. **Every state is derived from data that already exists.** Contract expiry
   and the usage period are computed at read time from
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
| Billing tenant | **Org** | Orgs already own projects, members and the admin boundary, and `usage_periods` already sums per org. One entitlement per org, allowance spanning all its projects. |
| Plan catalog | **Go, not rows** | A plan is (slug, name, free allowance, tier boundaries, retention) — static product config with revenue consequences: its tiers must match the meters on its provider product, so it belongs in review and deploy, not in a table an operator edits at 2am. It also means no seed step and no catalog row a signup could depend on. Provider product ids map to slugs in per-deployment config ([`payments.md`](payments.md) §16). |
| Repricing a plan | **Never in place — mint a new slug** (§4.2) | A Go catalog has no plan versions, so editing a sold plan's numbers re-splits every existing subscription on it, retroactively, on deploy. A commercial change disguised as a one-line edit is the most dangerous thing this design could allow. |
| Money amounts | **None in pug** ([`payments.md`](payments.md) §4) | Every rate — the plan's and a deal's — lives on the provider's product, the only thing that can charge it. A copy in pug would be a second authority that goes stale the first time a product is repriced. An operator may still write an agreed amount into `note`, which is prose no query reads as a number. |
| Entitlement changes | **Append-only history** (§5.1) | Invariant 4. |
| Negotiated deals | **A provider product, a pinned base plan and overrides on the org's own row** (§4.1) | A deal's price is its product, which is made against one plan's tiers. What pug keeps is that plan, pinned so a reprice cannot move it, and the deal's allowance, retention, name and term, as nullable columns on the org's own row. |
| Entitlement state | **Derived, never stored** | A `status` column is a second source of truth that can disagree with the rows beside it, and keeping it honest costs a worker. |
| Quota window | **Billing anniversary**, anchored to `orgs.create_time` | An org's month runs from the day it signed up. The alternative — a calendar month — is one line of code cheaper but resets everyone on the 1st regardless of when they signed up. §6.1. |
| Anchor representation | **Day-of-month integer, UTC midnight** | The meter's period sum is exact only for midnight-aligned windows. An anchor stored as an instant would silently drop a partial day from the total while leaving it in the daily series. §6.1. |
| Retention | **A day count on the plan, plus a per-org override** (§4) | How long history is kept is a term of the agreement like the allowance, so it sits beside it, is pinned immutable (§4.2) and is negotiable per deal. Days, not months: whatever eventually enforces this will subtract from `now`, and `AddDate` normalises `Feb 31` into March. Nothing subtracts today and nothing deletes — §13. |
| Unpaid orgs | **The free allowance, no trial** | Every org without a subscription gets the current plan's allowance, and a banner beyond it. No row, no provider object, no card. |
| Allowance audience | **Every org member** | Reads sit on the viewer floor, exactly like `ResourceUsage`: the person who notices the limit is rarely the admin. |
| Enforcement | **None** | Invariant 1. |
| Grant mechanism | **CLI only** | pug has no staff/superadmin concept, and inventing one to put an allowance field on a web page is not worth the auth surface. `pug billing` sits at the same trust level as `pug postgres migrate`. |

## 4. The plan catalog

`internal/core/billing/entitlement/plans.go` — an ordered slice of
`Plan{Slug, DisplayName, FreeEvents, TierUpTo, RetentionDays, Retired}`, with
`PlanBySlug` for lookup, `CurrentPlan` for the newest plan on sale and `TiersFor`
for the layout a subscription on a catalog slug is split by. A deal's layout is
`TiersFor` of its base plan (§4.1); free has none.

| slug | name | free events / month | tier upper bounds | retention |
|---|---|---|---|---|
| `usage-2026-10` | Pay as you go | 100,000 | 2M · 15M · 50M · 100M · 250M · unbounded | 365 days |

**The numbers are placeholders.** The commercial split is not decided. Each number
is a named constant in `plans.go`, so setting it is a one-line change there, the
matching meters on the provider's product, and `TestCatalogIsPinned`'s
expectation.

- **A plan is quantities, never money.** `FreeEvents` is the allowance: never
  reported to the provider, so never billed. `TierUpTo` splits the rest — tier k
  holds the events between the previous bound and its own, and the last tier is
  unbounded. The first tier starts where the allowance ends, so a tier whose bound
  an allowance override passes bills nothing. The provider's product holds one
  meter per tier and each tier's rate ([`payments.md`](payments.md) §4.1), and an
  hourly pass states each subscriber's per-tier counts to it (§4.2 there).
- **`free` and `custom` are states, not catalog entries.** `free` is an org with no
  live subscription: the current plan's allowance and retention, a banner beyond
  it, never a bill — so no tier layout either. `custom` is a negotiated deal
  (§4.1), split over its base plan's tiers at its own product's rates.
  `PlanBySlug` and `TiersFor` know neither.
- **`RetentionDays`** is how long the plan keeps history, in days at
  `RetentionYearDays` (365) flat per year. Nothing prunes on it (§13): it is a
  number pug renders, and every deployment over-delivers by keeping everything.
- **`Retired`** marks a plan that is never sold again. It stays in the catalog
  forever so its subscribers, and every deal pinned to it, keep resolving on its
  own numbers (§4.2); `OnSale` is `!Retired`, and checkout refuses a retired plan.
- **The catalog is checked at wiring time** (`validateCatalog`): a plan on sale
  exists, no plan uses a state's slug, and every plan's bounds rise strictly above
  its allowance. `NewService` fails on a malformed catalog rather than a request
  panicking on one.
- **The marketing site's pricing page is a second copy of the tiers** — and of the
  rates, which live only in the provider. Nothing enforces that they agree.
- **An absent allowance is not a plan.** It is what billing switched off, or a
  subscription or a deal's pin naming a slug the catalog lost, means on the wire
  (§7); every catalog plan has a finite one.
- Adding a plan needs no migration: it is a Go const and its `TestCatalogIsPinned`
  entry, a provider usage product with `Tiers()` meters
  ([`payments.md`](payments.md) §4.1), and a `PUG_DODO_PRODUCT_<SLUG>` key in each
  deployment (payments.md §13). The columns that name a plan —
  `billing_subscriptions.plan_slug` and a deal's `base_plan_slug` — carry **no**
  check constraint: a list of plans in a migration would be a second catalog to
  keep in lockstep. The row's own `plan_slug` is checked, because it holds a
  state, never a plan (§5).

### 4.1 Negotiated deals

A deal we agree with one customer — "Acme, $400/mo plus $9 per million, annual" —
is **its own provider product** plus the org's row. The product carries one meter
per tier of the plan it is made against, at the deal's rates, and the deal's
monthly fee; an operator creates it by hand in the provider's dashboard and pastes
its id ([`payments.md`](payments.md) §5). The row carries what pug owns:

| Field | Column | NULL means |
|---|---|---|
| Product | `provider_product_id` | not a deal |
| Base plan | `base_plan_slug` | not a deal |
| Allowance | `included_events_override` | the base plan's allowance |
| Retention | `retention_days_override` | the base plan's retention |
| Display name | `display_name_override` | "Custom" |

Term is `contract_ends_at`, and the paperwork lives in `note`. Nothing about a
deal needs a deploy or a catalog entry.

**A deal is exactly a product and a base plan.** `custom_needs_product` holds
`(plan_slug = 'custom') = (provider_product_id is not null)`: a deal's price is its
product, so a deal cannot exist without one, and a product names a deal and nothing
else. `custom_needs_base_plan` holds the same of `base_plan_slug`, the catalog plan
the product's meters were made against. No flag writes it: `pug billing set` pins
the plan current when the product is set, on a new deal and whenever the product
changes, and a renewal on the same product keeps its pin (§8). That pin is why a
reprice never moves a deal (§4.2).

The allowance is optional; a deal that charges from the first event sets
`--events 1`. `--events 0` is refused on a deal (`ErrDealAllowanceZero`): clearing
the override would fall back to the base plan's allowance, the opposite of what it
reads as.

**The deal's money is deliberately absent from pug.** pug stores what is free and
how the rest splits; the provider, the only system that can charge, holds the
rates ([`payments.md`](payments.md) §4). An agreed amount goes in `note` if an
operator wants it written down, which is honest about being a record rather than
a source of truth.

**A comp is an override on `free`, not a grant of a plan:**
`pug billing set --plan free --events 5000000 --until 2027-01-01` gives a larger
allowance until the date, and nothing bills it.

**Where this stops being the right shape:** overrides describe *one* org. Sell the
same bespoke terms to twenty customers and it has stopped being a deal and become a
plan, which belongs in the catalog (§4).

**A plan entitles an allowance and nothing else.** There are no plan-gated features
in pug, so a deal cannot grant one. If features ever become plan-scoped, that is a
new decision, not an override column.

### 4.2 Repricing, and why plans are immutable

A Go catalog has no plan versions. Editing a sold plan's allowance or a boundary
re-splits **every existing subscription** on it, retroactively, the moment the
deploy lands — and against meters on the provider's product that still expect the
old split. This is the one failure mode a rows-based catalog handles for free (the
archived design's `PLAN_STATUS_ARCHIVED` existed for exactly this), so a Go
catalog has to buy it back with a rule:

> **A plan's `FreeEvents`, `TierUpTo` and `RetentionDays` are immutable once any org
> holds it.** Repricing mints a new slug — `usage-2027-01` — with its own provider
> product, and marks the old one `Retired: true`. New rates alone are a new product
> under a new slug too. Nothing is deleted from the catalog while anything can
> still hold it.

Retention most of all: moving a boundary changes what a customer pays for events
they have not sent yet, while cutting its retention is a promise to delete what
they already did.

Existing subscribers keep resolving against the slug they hold and are unaffected;
new ones get the new plan. Grandfathering is then the default rather than something
an operator has to remember, and moving a customer onto new terms becomes what it
should be — a deliberate plan change at the provider, which the webhook records on
the subscription row.

A deal is unaffected too, because of its pin: it splits over the base plan on its
row (§4.1), whose meters its product still carries, and a renewal on the same
product keeps that pin. Moving a deal onto new terms is a new product pasted with
`set`, which pins it to the current plan — once its subscription has ended, since
`set` refuses to swap the product under a live one (§8).

What stays editable: `DisplayName`, because renaming a plan changes nothing anyone
bought. What this costs: a catalog that only grows, and slugs that carry a date.

The one deletion so far is 021's: `trial`, `starter`, `growth` and `scale` left
outright rather than retiring, because no subscription on any of them was ever
live, so nothing could still hold one — and 021 moved every row naming one to
`free` (§5).

## 5. Storage

Migrations `019_create_billing_entitlements.sql`, `020_create_billing_payments.sql`
(the product column) and `021_usage_plan_entitlements.sql` (usage billing): the
entitlement itself, and the history behind it (§5.1). The entitlement is a 1:1
extension of `orgs`, so the org id is the primary key rather than a `char(20)` xid
of its own — one row per org is then structural rather than a constraint somebody
has to remember to add.

The table as it stands after 021, abridged — the `<> ''` checks on its text
columns are left out:

```sql
create table billing_entitlements (
  -- NULL means the anchor is orgs.create_time's day of month, which is the case
  -- for every org today (section 6.1).
  anchor_day smallint
    constraint billing_entitlements_anchor_day_check
      check (anchor_day between 1 and 31),
  -- The catalog plan a deal splits over, pinned when its product is set (021).
  base_plan_slug varchar(50),
  contract_ends_at timestamptz,
  create_time timestamptz not null default now(),
  -- NO price or currency column, deliberately (payments.md section 4): every rate
  -- belongs to the payments provider, the only thing that can charge it.
  display_name_override varchar(150),
  included_events_override bigint
    constraint billing_entitlements_override_check
      check (included_events_override > 0),
  note text not null default '',
  org_id char(20) primary key references orgs(id) on delete cascade,
  -- free or custom: a state, never a plan (the state check below).
  plan_slug varchar(50) not null,
  -- The provider product a deal is bought against (020).
  provider_product_id text,
  -- How far back this org's events stay queryable. NULL means the plan's own
  -- retention. Nothing deletes on it (section 13).
  retention_days_override bigint
    constraint billing_entitlements_retention_check
      check (retention_days_override > 0),
  update_time timestamptz not null default now(),
  -- A deal's price is its product (021). Enforced here rather than in the CLI:
  -- the row is what every read trusts.
  constraint billing_entitlements_custom_needs_product
    check ((plan_slug = 'custom') = (provider_product_id is not null)),
  -- A deal splits over the plan its product was made against (021).
  constraint billing_entitlements_custom_needs_base_plan
    check ((plan_slug = 'custom') = (base_plan_slug is not null)),
  -- States do not change on a reprice, so this is no second catalog (021).
  constraint billing_entitlements_plan_slug_state_check
    check (plan_slug in ('free', 'custom'))
);
```

- **`plan_slug`** — `free` or `custom`, the only two slugs `SetPlan` writes (§8),
  and since 021 the only two the column takes (`plan_slug_state_check`). The row
  never grants a plan: a usage plan is held only through a subscription (§6), so
  the slug records what an operator staged — a comp on `free`, or a deal on
  `custom` that is waiting for its subscription or covered by one. The check is no
  second catalog, because states do not change on a reprice; a list of *plans*
  would be, which is why the columns that name one carry none (§4).
- **`anchor_day`** — the day of month the org's usage period starts on. NULL
  means `orgs.create_time`'s day, which is every org today (§6.1). It is a
  day-of-month integer rather than a date or an instant so that a period can only
  ever start at UTC midnight, which is what the meter's sum requires.
- **`contract_ends_at`** — when the row's negotiated terms lapse: past it the
  overrides stop applying (§6), except under the live custom subscription they
  describe. NULL means open-ended. It is the end of the *deal*, deliberately not
  the end of a usage period — an annual contract ending in March does not make
  March's usage period a year long. Keeping the two apart is why `anchor_day`
  exists as its own column rather than being read off whichever date happens to
  be nearby.
- **The `*_override` columns** — a deal's or a comp's allowance, retention and
  name (§4.1). NULL means "use the plan's" in each case. `included_events_override`
  and `retention_days_override` are checked `> 0` because 0 would read as an
  allowance or a retention of zero rather than as "no override", and because
  "unlimited" is deliberately not expressible here. A zero retention would be the
  worse of the two: it is the one value that could ever be read as "delete
  everything".
- **`provider_product_id`** — the product a deal is bought against, pasted by
  `--provider-product`. It is what makes `custom` purchasable, and what a deal's
  deliveries are mapped through ([`payments.md`](payments.md) §5).
  `custom_needs_product` ties it to `custom` in both directions.
- **`base_plan_slug`** — the catalog plan a deal splits over (§4.1), tied to
  `custom` in both directions by `custom_needs_base_plan`. No flag writes it:
  `set` pins it with the product (§8), because the product carries one meter per
  tier of that plan and a reprice must not move it (§4.2).
- **`note`** — the operator's record of why ("annual wire, INV-123"). Never
  returned by any RPC; it is for `pug billing show`, which prints it on the
  stored row, and for `show --history`.
- **No `status`, no `provider`, no `id`.** The first is derived (§6), the second
  has nothing to distinguish yet, the third has no use when `org_id` is unique.

019 seeds nothing and backfills nothing: every org that existed got its correct
entitlement from invariant 2 the moment the code deployed. 021 rests on one
premise: the fixed-price tiers (`trial`, `starter`, `growth`, `scale`) leave the
catalog outright rather than retiring, because no subscription on any of them was
ever live, so nothing can still hold one — the one deletion §4.2 allows. Before it
changes a row it **fails**, naming every row it cannot place with its plan and
product — a `custom` row with no product, a product on any other row, or a slug
that is neither `free`, `custom` nor a removed tier — rather than guess which half
is wrong; Postgres alone would name only the constraint. The operator fixes each
with `pug billing set` and re-runs. It then rewrites a removed tier's row to `free`
— nothing billed it without a subscription, and its overrides keep resolving on
free — pins every existing deal to `usage-2026-10`, and appends a history snapshot
(actor `migration/021`) for every row it changes, so invariant 4 holds for the
migration too. `Down` reverses the schema; the rewrites, and the snapshots that
record them, stay. Check a deployed database before it runs.

### 5.1 History

Every write to `billing_entitlements` appends a full snapshot of the row as it
now stands, in the same transaction:

```sql
create table billing_entitlement_history (
  actor varchar(150) not null,
  changed_at timestamptz not null default now(),
  id char(20) primary key,
  -- The entitlement as of this change, verbatim. NULL across the value columns
  -- is a deletion: the org returned to free with no row.
  anchor_day smallint,
  base_plan_slug varchar(50),
  contract_ends_at timestamptz,
  display_name_override varchar(150),
  included_events_override bigint,
  note text not null default '',
  -- No FK: the history outlives the row, and an org's terms are still the
  -- answer to a question after the org is gone.
  org_id char(20) not null,
  plan_slug varchar(50),
  provider_product_id text,
  retention_days_override bigint,
  -- Kept by 021 for snapshots recorded while the trial ran; nothing writes it now.
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
  refuses the command without it. The payments side never writes this table —
  it writes `billing_subscriptions` ([`payments.md`](payments.md) §6) — so every
  actor is a person, bar 021's `migration/021` (§5). It is stated rather than
  detected because these commands run from a pod, where the OS user is the
  image's uid and reads the same for every operator. An unattributed change to a
  commercial agreement is barely better than no record, so there is no default
  and no nullable column to leave empty.
- **No foreign key to `orgs`.** `billing_entitlements` cascades away with the
  org; the history must not, because "what were they on when they left" is
  precisely a question asked after deletion — in a refund dispute, most often.
- **A snapshot is kept as it was written.** The table carries the row's own
  checks, so a snapshot is one a live row could have held — but 021 added
  `custom_needs_product` and `custom_needs_base_plan` here `not valid`, so a deal
  recorded before usage billing, with an events override and no product or pin,
  stays as it was. The rules bind new rows only, and the state check is not here
  at all: a snapshot may name a tier the catalog has since lost. 021 kept
  `trial_ends_at` for the same reason, and `show --history` prints it as
  `trial-ends=`.
- **Append-only by convention, and nothing in the codebase updates or deletes
  it.** There is no RPC that reads it either: it is operator and support data,
  reached through `pug billing show --history`.

## 6. Resolution

`entitlement.Resolve(orgCreateTime, rec, sub, now, billingEnabled)` is a pure
function returning the resolved `Entitlement` — slug, display name, status, the
free allowance `IncludedEvents`, `RetentionDays`, the tier layout `TierUpTo`, the
contract date and the period bounds, plus the live subscription's status, period
end and customer. `sub` is the live provider subscription, nil for most orgs
([`payments.md`](payments.md) §7). No I/O, so the whole rule set is
unit-testable without a container. `orgCreateTime` is an argument rather than
something the package looks up because it is the default quota anchor (§6.1).

In order:

1. **Billing disabled** (§9) → status `FREE`, plan `free`, and no allowance,
   retention or tiers. A self-hosted install has no allowance at all, so no
   banner can fire even if a client forgets to check the flag. The switch fails
   *open* on the number because the number enforces nothing.
2. **No live subscription** → `FREE` on the current plan's allowance and
   retention, whatever the row says, and no tiers: nothing bills a free org, so
   nothing splits its usage. Derived, never materialized: a read must not
   write a row. The row never grants a plan on its own, because without a
   subscription nothing bills: a comp is a bigger allowance on free, and a
   `custom` row still waiting for its subscription resolves plain free — free's
   name, allowance and retention — while its terms wait on the row.
3. **A live `custom` subscription** → `ACTIVE` as "Custom", on its base plan's
   allowance, retention and tiers: the plan pinned on its row (§4.1), retired or
   not, whichever plan is current. A deal is priced by its own product over the
   layout that product's meters were made for, and its row supplies whatever it
   negotiated on top.
4. **A live subscription on a catalog slug** → `ACTIVE` on that plan, retired or
   not.

A subscription is live while it is `active` or `past_due`
([`payments.md`](payments.md) §7). A cancelled row is kept — "when did this
lapse" is a question support asks — but never consulted.

Then each present override replaces the corresponding field of the resolved plan
(§4.1) — allowance, retention and display name — patching whatever steps 2–4
produced. A deal survives a catalog reprice untouched because step 3 reads its
pin, not the current plan (§4.2).

**A deal's terms are held only through its own subscription.** A `custom` row's
overrides apply only while a live custom subscription exists: staged and not yet
bought, the org is plain free (step 2), and beside a usage plan it bought
instead it is on that plan's own numbers, whether or not the deal's contract has
lapsed. The deal's allowance riding on a usage plan would go unbilled at usage
rates.

A comp's overrides are gated on the **contract**, not on the resolved slug: a
lapsed `contract_ends_at` drops them, and a comp with no contract date keeps them
however the plan resolves. So an open-ended comp on free rides onto a usage plan
the org later buys; for a comp, `applyOverrides` does not compare `plan_slug` to
the plan in force.

The one exception to the contract gate runs the other way: a live **custom**
subscription keeps its overrides past `contract_ends_at`, because that date bounds
an operator's grant and must not strip the terms of a deal somebody is being
charged for.

Whether a plan-in-force gate *should* exist for a comp is open; a deal has one
already, its own subscription. Nothing is enforced on an allowance, but the usage
meter splits a subscriber's bill from the resolved one ([`payments.md`](payments.md)
§4.2), so the gap costs revenue as well as a number on a page: an open-ended comp
rides onto a subscription bought after it, and its extra allowance goes unbilled.
Until a gate exists, give a comp an `--until`.

An **unknown slug on a live subscription** — only reachable if a slug is removed
from Go while a subscription still names it — resolves `ACTIVE` under that slug,
with no allowance, retention or tiers. A live deal whose pin names a plan the
catalog does not know resolves the same way, as "Custom". No override patches
either: an allowance with nothing to split it by is exactly the guess this
avoids. `Resolve` stays pure, so the log happens in `Service.GetEntitlement`,
which has a ctx to attach it to; it is a `WarnContext` and deliberately **not** a
`telemetry.RecordError`, which would record an exception on every dashboard load
for as long as the drift lasts. Failing to the free allowance would tell a paying
customer they are over their limit; failing to "no allowance" is a silent banner
and a log, and leaves nothing to split the org's usage by. Nothing but review
makes this unreachable: §4.2 deletes a slug only once nothing can still hold it.

**Expiry is lazy, always.** A contract that ended an hour ago drops its overrides
on the next request, with nothing having run in between. This is the whole
reason §2's third invariant is worth holding: there is no job whose failure can
leave an entitlement stale, because there is no job.

### 6.1 The usage period is a billing anniversary

**An org's month runs from its anchor day, not from the 1st.** An org that
signed up on the 17th has periods `17 Jan → 17 Feb → 17 Mar`, and that is the
window both halves of "X of Y" are measured over.

#### Where the anchor comes from

`orgs.create_time`. Every org has one, and it has been NOT NULL since migration
001, so the default anchor needs no column, no operator action and no backfill: an
org that has never touched billing still has a well-defined window, which keeps
invariant 2 intact.

`billing_entitlements.anchor_day` (§5) overrides it, and is NULL for almost
every org. It was added because checkout had to choose between two ways of
aligning a real charge date, and a nullable column deferred that choice at no
cost:

- **align the provider to us** — set the subscription's billing cycle to the
  org's existing anchor, so the invoice date and the quota reset are the same day
  forever, and `anchor_day` stays NULL. This is the better system and the
  intended one.
- **align us to the provider** — write the charge day into `anchor_day` at
  checkout. Simpler, but it moves the anchor mid-life, which truncates one period
  and gives that month a short window.

Checkout shipped with neither ([`payments.md`](payments.md)): the provider bills
on its own cycle from the checkout day, only an operator writes `anchor_day`, and
the two dates differ. The usage period drives the banner and `GetUsage`; what a
subscriber is billed follows the provider's period instead.

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
- **`usage.Service.GetOrgPeriod`** is the single derivation both `GetUsage`'s
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
`usage_daily` keeps day grain and re-sums any window inside the 390-day
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

`proto/dashboard/billing/v1/billing.proto` — `BillingService`, JWT boundary.
`org_id` is on the request so `authzspec.OrgFromMessage` resolves the org
through the generated `GetOrgId()`. This section describes the entitlement-only
slice; payments added four more RPCs, see payments.md §12.

| RPC | Spec | Returns |
|---|---|---|
| `GetBillingStatus` | `OrgGated(ResourceBilling, ActionRead)` | `billing_enabled`, plan (slug, display name), derived status, `included_events`, `retention_days`, `contract_ends_at`, `period_start`, `period_end` |

The plan fields are the **resolved** ones — overrides already applied (§4.1), so
a client never reconstructs a deal from a base plan plus patches. `note` and the
history never cross the wire; both are operator data. No price crosses it either:
every rate lives on the provider's product.

- **No consumption number.** The client makes two calls —
  `UsageService.GetUsage` for X, `GetBillingStatus` for Y. Folding usage into
  this response would make billing depend on the usage subsystem and would have
  to restate its three-state freshness contract (absent / computing / really
  zero), which is exactly the kind of duplicate that drifts.
- **`included_events` is the free allowance, and absent-able** — absent means *no
  allowance*, never zero. It is a `google.protobuf.Int64Value` wrapper, not a
  bare edition-2023 scalar: protoc-gen-go would give the scalar presence, but
  protoc-gen-es renders it as a NON-optional bigint, so absence would reach the
  dashboard as `0`. This is the one place billing diverges from
  `GetUsageResponse.used_events`, which is a bare `int64` a client can pair with
  `usage_computed_at` to detect absence — the allowance has no such companion
  field. A client consuming it from `../app` must check presence rather than
  truthiness.
- **`retention_days` is absent-able on the same terms**, and is the same
  `Int64Value` wrapper for the same reason — absent is *no bound*, and "0 days of
  history" is the one thing it must never say. It states what the plan promises,
  not what has been deleted: nothing prunes on it (§13), so a client must not
  render it as "data older than this is gone".
- **Removed fields are reserved, not deleted.** `trial_ends_at` (5) on the
  response, and `price_cents` and `currency` (3, 4) on both `Plan` and
  `PlanOption`, are reserved by name and number, so no later field can reuse
  either with another meaning. `BILLING_STATUS_TRIALING` stays in the enum,
  deprecated and never emitted: buf's `FILE` rules forbid deleting an enum value,
  and `buf.yaml` relaxes only field deletion, only for `proto/dashboard`.
- **`tier_usage` came with usage billing**: each tier's count for the live
  subscription's current period as last stated to the provider, carry included,
  beside the tier's upper bound (0 for the unbounded last), with
  `tier_usage_as_of`. Absent until the period's first statement
  ([`payments.md`](payments.md) §4.2).
- **No `ListPlans`** in this slice — a price list whose buy button does not exist
  yet is a dialog that can only disappoint. It arrived with checkout; see
  payments.md §12.
- **Read-only, so read-only permissions.** `authz.ResourceBilling` is added to
  the const block **and** `allResources` (`policy_test.go` fails a
  declared-but-ungranted resource), with `grant(roleViewer, ResourceBilling,
  ActionRead)` putting it on the viewer floor for member and admin to inherit.
  In this slice no create/update/delete action is granted, because no RPC
  performs one; payments adds `ActionCreate` for admins (payments.md §12).

All three wiring points are enforced at build or startup: the entry in
`authz_served.go` and the procedure entry in `authz_registry.go` fail a contract
test if missed, and `handle(...)` in `server.go` records the mounted service so
`assertServedServicesMatch` fails startup on a service that is served but not
mounted.

## 8. Operator CLI

```shell
pug billing show <org-id> [--history]
pug billing set  <org-id> --plan free|custom --actor <who> [--events N]
                          [--retention-days N]
                          [--name "Acme Enterprise"] [--anchor-day 17]
                          [--until 2027-01-01] [--note "INV-123"]
                          [--provider-product prod_2f9k...]
pug billing clear <org-id> --actor <who>
```

Postgres only — no provider, no network. `set` upserts the row, `clear` deletes
it (returning the org to the free allowance), and `show` prints the resolved
entitlement *and* the stored row beneath it, since the interesting bugs live in
the gap between them — a lapsed deal's allowance, like a staged one's, is
invisible in the resolved answer but still carries onto the next `set`.

`set` writes `free` or `custom` and nothing else. A usage plan is held only
through a subscription (§6), so its slug is refused (`ErrPlanNotAssignable`)
rather than stored as a plan nothing bills. `SetPlan` and the CLI read the two
from one list, `entitlement.AssignableSlugs()`.

Three rules the flags do not spell out:

- **`--until` is inclusive of the date given.** The resolver's comparison is
  half-open, so `ContractEndExclusive` — beside that comparison, not in the CLI —
  stores the *following* midnight: `--until 2026-12-31` means the terms run
  through all of 31 December. `show` prints the stored instant, which is
  therefore the 1st.
- **`free` without `--until` ends the deal.** It clears the contract, the
  overrides the contract gated and the product: the terms belonged to the deal,
  and a product left behind would keep offering a buy button for it. With a real
  `--until` it is a comp, and keeps the overrides it is given.
- **A deal's base plan is pinned, never passed.** `set` writes `base_plan_slug`
  itself (§4.1): the current plan on a new deal and whenever the product changes,
  the stored pin on a renewal with the same product, and nothing once the row
  leaves `custom`. `show` prints it as `base plan` in `STORED`, and the history
  line carries `base=<slug>`.

A negotiated deal (§4.1) is one `set`, once its product exists in the provider:

```shell
pug billing set o_2f9k --plan custom --provider-product prod_2f9k \
                       --events 5000000 --retention-days 2555 --name "Acme Enterprise" \
                       --until 2027-01-01 --note "INV-123" --actor "praveen/INV-123"
```

There is no `--price`: what the deal is charged lives on its product (§4.1), and
`--note` is where an operator writes an agreed amount down.

`--events`, `--retention-days`, `--name` and `--anchor-day` write the override
columns; omitting one on a re-`set` leaves the stored value alone, and passing
the empty value (`--events 0`, `--retention-days 0`, `--name ""`,
`--anchor-day 0`) clears it back to the plan's — except `--events 0` on a deal,
which is refused (§4.1). Leaving them alone is the right default because the
common re-`set` is a renewal — a new `--until` on terms that have not changed —
and a flag that silently reverted a customer's negotiated allowance to a catalog
number would be the most expensive bug this CLI could have.

Guards on `set`, all refusing rather than guessing: an unknown slug
(`ErrPlanNotFound`); a usage plan's slug (`ErrPlanNotAssignable`); `custom` with
no product (`ErrCustomNeedsProduct`), and a product on anything but `custom`
(`ErrProductNeedsCustom`) — so a comp that follows a deal clears the product with
`--provider-product ""` rather than have it guessed away; `--events 0` on a deal
(`ErrDealAllowanceZero`); and leaving `custom`, or swapping its product, while a
live custom subscription maps its renewals through the stored product
(`ErrClearWouldStrandSubscription`), which `clear` refuses too. The product
guards are checked against the *merged* row, not the flags, so a re-`set` on a
deal needs no `--provider-product`, and their messages name the flag that fixes
them. `--events` refuses a negative rather than reading it as a clear, and
`--anchor-day` is range-checked in both the CLI and the service.

Every write appends to the history (§5.1) in the same transaction, attributed to
the `--actor` it was given. `show --history` prints the org's
changes newest-first, which — with `note` — is what a refund or renewal argument
is actually settled from.

This CLI is why the slice is usable rather than decorative — without a writer,
the table is dead and the RPC only ever reports free. If it should be smaller,
the honest minimum is `show` + `set`; `clear` is a convenience over the same row.

Every command prints the same report — the org, the switch, `RESOLVED`, `STORED`,
`SUBSCRIPTIONS` and optionally `HISTORY` — so a write is confirmed by the state it
produced rather than by an "ok". Three things about that report are load-bearing:

- **An absent value prints `(none)`, never `0`.** Absent `included_events` means
  NO allowance (§7); a zero would state a billing figure the deployment never
  claimed. `tiers` prints the resolved split — a subscriber's `100,000 free ·
  ≤ 2,000,000 · … · beyond` — listing only the bounds above the allowance, by the
  meter's rule (§4): the deal above, once bought, prints `5,000,000 free ·
  ≤ 15,000,000 · … · beyond`. It prints `(none)` when nothing splits the usage:
  free, which nothing bills, an unknown plan, or billing off.
- **A mutation's `STORED` half is the row its own transaction wrote**, not a
  re-read. The reader is a replica in principle, and confirming a write against a
  lagging read is how a successful `set` prints the row it replaced. `RESOLVED`
  is re-read, because it needs the subscription (payments §7) as well.
- **`--provider-product` is the one field that decides whether an org can spend
  money** (payments §5.2), so it is printed in `STORED` and carried in the
  history line.

The report goes to stdout and the logs to stderr, so `show` stays pipeable; a
refusal is a non-zero exit with the reason on stderr and no usage block.

## 9. Configuration

| Var | Default | Meaning |
|---|---|---|
| `PUG_BILLING_ENABLED` | `false` | The single switch. Off ⇒ `billing_enabled=false` and no allowance anywhere (§6). Set it on every pod of a billed deployment. |

It follows `PUG_DEMO_ENABLED` exactly: `envconfig` on the server, which rejects
a malformed bool outright. There is no worker and no CLI gate — `pug billing`
edits rows whether or not the server is serving them, which is what makes it
usable to prepare a deployment before the switch goes on.

## 10. Testing

`internal/core/billing/entitlement` and `internal/core/billing/subscription` each
declare `func TestMain(m *testing.M) { testutil.Main(m) }` and use no
`t.Parallel()` for the container-backed cases ([`CLAUDE.md`](../../CLAUDE.md)
§ Testing); the root package has no tests. `Resolve` itself is pure, so the rule
table is a plain unit test.

- **Resolution** — no row resolves free on the current allowance, with no tiers,
  and writes nothing; a live subscription supplies its plan and outranks the row,
  a cancelled one supplies nothing, and a custom one splits over its base plan's
  tiers; a staged deal resolves plain free, and a deal's terms never ride onto a
  usage plan bought instead; a contract past `contract_ends_at` drops its
  overrides with no sweep having run, except under the live deal it describes;
  each override patches only its own field; a reprice leaves a live deal on its
  pinned plan (`TestARepriceDoesNotResplitALiveDeal`), which a renewal on the
  same product keeps and a new product moves
  (`TestADealKeepsItsPinUntilItsProductChanges`); an unknown subscription slug
  keeps its name with no allowance, retention or tiers, even under a comp, and a
  deal pinned to an unknown plan resolves the same; billing disabled resolves to
  no allowance regardless of the row or the subscription.
- **Retention** — the plan resolves its own value; a negotiated
  `retention_days_override` wins and lapses with its contract; an unknown plan —
  a subscription's slug or a deal's pin — and a disabled deployment both report
  *no bound* rather than a year, which is the same fail-open direction the
  allowance takes.
- **The free corners**, which is where a comp lives: `free` without `--until`
  ends a deal's contract, overrides and product; `free` with a date is a comp and
  keeps them; and a comp's `contract_ends_at` still expires them, so a
  time-boxed pilot lapses like any other deal.
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
- **Storage** — every catalog slug fits the columns that store it, though
  `SetPlan` refuses to assign one, which is the test that catches a slug
  outgrowing `varchar(50)`: the columns that name a plan deliberately carry no
  check (§4). A `custom` row without a product or a base plan, either of them on
  any other row, and a `plan_slug` that is not a state are rejected by the
  database, not merely by the service.
- **History** — every mutating CLI path appends exactly one snapshot in the same
  transaction, `clear` included; a failed write appends nothing; the history
  survives its org being deleted. The last of those is the one a foreign key
  would quietly break, so it is a test rather than a comment.
- **Migration 021** — stepped back to 020 with `testutil.PostgresMigrations` and
  seeded in the old shape: it names every row it cannot place and applies
  nothing (`TestMigration021NamesEveryRowItCannotPlace`), and records every row
  it rewrites while the history keeps a trial's end
  (`TestMigration021RecordsWhatItRewrites`).
- **Catalog** (§4, §4.2) — `TestCatalogIsPinned` pins every plan's `FreeEvents`,
  `TierUpTo`, `RetentionDays` and `Retired`, so editing a sold plan fails CI and
  the fix is to mint a new slug. It is the only guard against a one-line
  allowance cut or a moved boundary, since nothing else in the system can tell an
  intended reprice from a typo. `validateCatalog` is tested against a plan named
  like a state, a duplicate, and bounds that fail to rise; the accessors return
  copies, so a caller cannot re-split the catalog for the whole process.
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
   log in never sees a banner, so the allowance warning has to reach them; a banner
   alone is a notification only for people already looking.
2. **Checkout** (payments provider) — **shipped**, and designed in
   [`payments.md`](payments.md), which supersedes this list where the two differ:
   products, checkout sessions, a signed webhook inbox, a reconcile pass and the
   provider-reported states this slice has no way to derive (`PAST_DUE`,
   `CANCELLED`), in tables of its own. The catalog stayed in Go; what cannot be a
   Go const is the per-environment provider product id, and it lives in config.
   Usage billing (2026-09-27) builds on it: one usage plan whose rates live on the
   provider's product, and an hourly pass that reports each subscriber's usage to
   that product's meters, one per tier ([`payments.md`](payments.md) §4.2). Four
   things had to be decided *in* checkout rather than discovered after it:
   - **The provider is a merchant of record** (Dodo, Paddle, Lemon Squeezy) —
     which is what makes VAT/GST registration, tax collection and legally
     compliant invoices somebody else's obligation. This is an architectural
     dependency, not a vendor preference: moving to a payments-only processor
     later makes worldwide tax pug's problem, and there is nothing in this design
     that would absorb it.
   - **Deleting an org must cancel at the provider first.** The FK cascade drops
     the entitlement row and the provider knows nothing about it, so a design
     without it would leave a deleted customer being charged — a refund and a
     chargeback, not merely an inconsistency.
   - **Dunning**: what a failed renewal does to entitlement (decided: nothing —
     `PAST_DUE` keeps the plan; degrading a paying customer's product over an
     expired card is worse than a few unbilled days), and how many notices go out
     before it lapses.
   - **Annual terms.** Standard, and it interacts with §6.1: an annual contract
     renews yearly while the usage period stays monthly, so `contract_ends_at`
     and the anchor do different jobs and both are needed.
3. **Retention enforcement** — the prune that makes §4's number more than a
   promise. It is deliberately its own slice because it is the first thing in
   this subsystem that would *destroy* customer data, and three things have to be
   decided in it rather than discovered after: what a downgrade or a lapse does
   to history already stored (proposed: a grace period and a notice, never an
   immediate delete — §12), whether the bound is a ClickHouse TTL or a job that
   can be halted, and what stops a resolution bug from deleting on a wrong
   number. Until it exists pug keeps everything, which over-delivers on every
   plan.
4. **Payment ledger**: invoices, recorded manual payments, refunds and
   chargebacks, on a separate admin-only resource — amounts and invoice
   references do not belong on the viewer floor the allowance banner sits on. A
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
  events between quota periods. Accepted at these volumes.
- **A short month shortens the period.** An org anchored on the 31st gets a
  28-day window in February against the same monthly allowance (§6.1). Every
  anniversary billing system has this; the alternative — clamping the anchor
  permanently to 28 — quietly moves the reset date of every org that signed up
  late in a month.
- **Raising an allowance mid-period raises it for the whole period**,
  retroactively covering events already counted, because the allowance is
  resolved at read time and is not a running balance. Generous in the customer's
  favour on the banner.
- **A moved anchor truncates one period.** An `--anchor-day` change cuts the
  org's current window short once, and that month's number will look small next
  to its neighbours.
- **A lapse or a downgrade shortens retention retroactively.** A 10-year deal
  that ends resolves to the current plan's 365 days the same instant its
  allowance drops, so the *stated* bound moves across years of already-stored history at
  once. Harmless while nothing prunes; it is the specific reason enforcement
  (§11.3) needs a grace period rather than a nightly delete.
- **A retention "year" is 365 days flat**, so a 7-year term is ~1.7 days short of
  seven calendar years. Deliberate: the alternative is calendar arithmetic whose
  normalisation moves the boundary the wrong way, and the error is invisible next
  to a bound nothing enforces.
- Counting's own imprecisions — staleness, day-boundary rounding — belong to
  [`usage.md`](usage.md).

## 13. Deliberately not handled

Things a mature SaaS billing system has that this one does not. Each is a
choice, recorded so it stays one.

**Nothing bounds a runaway org, and nobody is told.** Enforcement is invariant 1
and stays that way, but the consequence is worth stating plainly: an org can send
fifty times its allowance, and the only signal is a banner shown to the person with
the least reason to act on it. That is a ClickHouse cost exposure and an abuse
vector, not a billing gap — the fix belongs with the meter, which already sweeps
every project on a schedule, and is noted in [`usage.md`](usage.md). It does not
need an allowance to be useful: "any org over N events/day" catches the same
traffic and works for custom deals too.

**Nothing enforces retention.** The plan's `RetentionDays` is a term with no
prune behind it: no ClickHouse TTL, no delete job, no query clamp, and no path
that reads the number for anything but rendering it. Invariant 1 keeps billing
out of *ingestion*; deletion is the larger promise, so it is §11.3's own slice
rather than a switch flipped here. The consequence is stated plainly: a customer
on a 1-year plan can still query year-old data, and pug pays to store it.

**Billing an org with no subscription.** Past the free allowance it sees a
banner and nothing else: no event is refused and nothing is invoiced. Usage
beyond the allowance is billed only to a subscriber, through its product's tier
meters ([`payments.md`](payments.md) §4.2); turning a free org into one is a
checkout, never an invoice pug raises.

**Per-seat pricing, add-ons, credits and account balances.** The product is
priced by events; members are free. Nothing here is close to needed, and each
would add a second dimension to an allowance that is currently one number.

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
the provider's customer portal ([`payments.md`](payments.md)); an
in-dashboard switcher is a later call.

## 14. Divergences from this design

Where the code differs from the sections above, the code wins and the reason is
here.

- **`Sellable` became `Retired`** (§4). One flag was conflating "may be granted"
  with "may be purchased", and `custom` needs the first whatever the second says
  — a negotiated deal is granted to an org that has never held one, so
  a `Sellable: false` guard made every custom deal impossible to create. Splitting
  them now would have shipped a purchasability flag with no consumer, so only the
  guard's own concept exists: `Retired`. Checkout brought the other half as
  `Plan.OnSale`, derived and never stored — now simply `!Retired`, since `free`
  and `custom` left the catalog. `custom` is still sold, to the one org whose row
  records its product ([`payments.md`](payments.md) §5.2), so a sale check that
  walks only the catalog misses it: `offers`, the one list `PlanOptions`,
  `Purchasable` and `CreateCheckoutSession` read, asks that row too.
- **The trial and the fixed-price tiers are gone** (2026-09-27, usage billing).
  The catalog is one usage plan of quantities, `free` and `custom` are states, a
  plan is held only through a subscription, and `extend-trial` went with the
  trial — as did `trial_ends_at` (the history keeps its column, §5.1), the list
  price on the wire, `MaxTrialDays`, `ErrTrialNotExtended`,
  `ErrTrialOnGrantedPlan`, `ErrCustomNeedsQuota` and `ErrPlanRetired`. A deal is
  defined by its product and its pinned base plan (`custom_needs_product`,
  `custom_needs_base_plan`) rather than by an events override.
- **A comp's overrides are gated on the contract date** (§6), so an expired 5M
  comp cannot keep its 5M for good. They are *not* gated on the resolved slug: an
  open-ended comp's numbers ride onto whatever plan is in force. A deal's are
  gated on its own live custom subscription instead.
- **The `GetUsage` RPC now answers `NotFound` for an unknown org.** It previously
  reported a metered zero for any id, because the period came from the clock
  alone and no lookup could fail. Resolving an anchor requires the org row, so a
  missing one is now a real error — same `ORG_NOT_FOUND` reason the orgs service
  uses.
- **`included_events` ships as an `Int64Value` wrapper** — as `price_cents` did
  until usage billing reserved it — not the bare edition-2023 scalar §7
  originally specified. protoc-gen-go would have given
  it presence, but protoc-gen-es renders a singular scalar as a non-optional
  bigint, so "no allowance" would have reached the dashboard as `0` — the one
  thing the field must never say. Verified against the generated TS.
- **`clear` distinguishes an unknown org from an org with no row.** It returned
  success for both, so a typo'd id printed "entitlement cleared" while the real
  org kept its deal.
- **`ListPlans` is absent as designed, but so is any RPC that reads the
  history.** §5.1 says the history is operator data; `pug billing show --history`
  is the only reader, and nothing serves it over the network. (`ListPlans` later
  arrived with checkout — payments §17.)
- **`show` prints the org's display name, which no billing query returns.** The
  CLI reads `orgs` directly for it. An operator pastes an id and is about to
  write to it; "Acme Inc" beside `o_2f9k` is the only thing in the report that
  catches the wrong org before the write, and it is cheap.
- **`--until ""` clears the contract end.** §8 lists the empty value of every
  other override as its clear but not this one, which left a deal's end date
  unremovable without a `set` back to free.
- **The CLI validates `--anchor-day` and `--events` itself**, as §8 says for the
  anchor day and does not for the allowance. Both are also checked in the service,
  which is what a second caller would hit; the CLI's copy exists so the message
  names the flag rather than the column.
