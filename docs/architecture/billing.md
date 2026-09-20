# Billing — entitlement

Design reference for the first billing slice (`internal/core/billing`,
`proto/dashboard/billing`, `pug billing`). Linked from the root
[`CLAUDE.md`](../../CLAUDE.md) — read this when working on plans,
quotas or trials. Event **counting** is not here: see [`usage.md`](usage.md).

> **Status: implemented**, except where §14 records a divergence: migration 019,
> the Go catalog, `Resolve`, the entitlement store, §7's `GetBillingStatus` RPC
> and §8's `pug billing`. The code is the authority; this document explains why
> it is shaped the way it is. This is the first of three billing slices (§11)
> and stands on its own, with no payments provider involved — the second,
> checkout, is [`payments.md`](payments.md) and supersedes §11's sketch of it.

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
| Rate card catalog | **Go, not rows** | A card is (slug, name, free allowance, tiers, retention) — static product config with revenue consequences, so it belongs in review and deploy, not in a table an operator edits at 2am. It also means no seed step and no catalog row a signup could depend on. Provider product ids have since arrived and it stayed Go: they map to slugs in per-deployment config ([`payments.md`](payments.md) §16). |
| Repricing a card | **Never in place — mint a new slug** (§4.2) | A Go catalog has no plan versions, so editing a sold card's numbers changes what every existing customer on it gets, retroactively, on deploy. A commercial change disguised as a one-line edit is the most dangerous thing this design could allow. |
| Money amounts | **The terms a period is priced *on* live here; what was *charged* lives at the provider** (§4.1) | Reversed from the original "no structured amount per org" — see §14. Pug computes the amount: a graduated card is a table only pug holds, so `Quote` is pug's arithmetic either way. A deal's fee and rate are therefore not a copy of the provider's number but the **input** that number is derived from, and without them a custom deal is an org nothing can price. What the provider actually took stays a separate, observed fact on `billing_subscriptions.price_cents`. Integer minor units + ISO 4217 throughout, because an amount without its unit is only unambiguous while there is exactly one currency. |
| Entitlement changes | **Append-only history** (§5.1) | Invariant 4. |
| Negotiated deals | **Terms and overrides on the org's own row** (§4.1) | A bespoke deal is a name, a price, an allowance and a term for exactly one org. Nullable columns on that org's row hold it, where a private-plan catalog or a discount percentage recombined with a base price would both need a table and a join to say the same thing. |
| Entitlement state | **Derived, never stored** | A `status` column is a second source of truth that can disagree with the timestamps beside it, and keeping it honest costs a worker. Every state this slice has is a comparison against `now`. |
| Quota window | **Billing anniversary**, anchored to `orgs.create_time` | An org's month runs from the day it signed up, which is the date its trial already runs from. The alternative — a calendar month — is one line of code cheaper but resets everyone on the 1st regardless of when they bought, which is a support conversation the day a card is charged. §6.1. |
| Anchor representation | **Day-of-month integer, UTC midnight** | The meter's period sum is exact only for midnight-aligned windows. An anchor stored as an instant would silently drop a partial day from the total while leaving it in the daily series. §6.1. |
| Retention | **A day count on the card, plus a per-org override** (§4) | How long history is kept is a term of the agreement like the quota, so it sits beside it, is pinned immutable (§4.2) and is negotiable per deal. Days, not months: whatever eventually enforces this will subtract from `now`, and `AddDate` normalises `Feb 31` into March. Nothing subtracts today and nothing deletes — §13. |
| Unpaid orgs | **14-day trial → free tier** | Trial is the org's age, not stored state: no row, no provider object, no card. |
| Quota audience | **Every org member** | Reads sit on the viewer floor, exactly like `ResourceUsage`: the person who notices the limit is rarely the admin. |
| Enforcement | **None** | Invariant 1. |
| Grant mechanism | **CLI only** | pug has no staff/superadmin concept, and inventing one to put a quota field on a web page is not worth the auth surface. `pug billing` sits at the same trust level as `pug postgres migrate`. |

## 4. The rate card catalog

`internal/core/billing/entitlement/catalog.go` — an ordered slice of
`RateCard{Slug, DisplayName, Currency, FreeEvents, Tiers, RetentionDays,
Retired}`, with `CardBySlug` for lookup and `CurrentCard` for the newest live
one. A `Tier` is `{UpToEvents, CentsPerMillion}`.

Pug sells **usage**, not seats or feature tiers: one card, priced in graduated
bands, and the only question is how many events an org sent.

`usage-2026-09-1` — "Usage", USD, 100,000 events free per period, 5 years retention:

| band | cents per million |
|---|---|
| first 100,000 | free |
| to 2,000,000 | $40.00 |
| to 15,000,000 | $28.00 |
| to 50,000,000 | $20.00 |
| to 100,000,000 | $14.00 |
| to 250,000,000 | $10.00 |
| beyond | $7.00 |

- **Bands are graduated, not volume.** Each band prices only the events that fall
  inside it; passing a threshold never reprices what came before. 10M events are
  100,000 free + 1.9M at $40/M + 8M at $28/M = **$300.00**, not 10M priced wholly
  at the $28/M band reached. This is the whole reason pug holds the card rather
  than the provider (§14): a provider that only expresses volume pricing cannot
  state this table.
- **`FreeEvents` is an allowance, not a quota.** Going past it costs money; it
  stops nothing (invariant 1). `free` is not a card — it is the name for what an
  org that pinned nothing gets, which is this card's allowance.
- `free`, `trial` and `custom` are **states, not cards** (`SlugFree`, `SlugTrial`,
  `SlugCustom`). `trial` is never stored — `extend-trial` writes a row with a
  `trial_ends_at`; `custom` names a deal whose numbers live on the org's row
  (§4.1).
- **`Currency` is mandatory and travels with every amount** (ISO 4217, `USD`
  throughout today). Tier rates are minor units of *that* currency, which is not
  always 1/100 — JPY has no minor unit, KWD has three — so nothing may assume
  cents when formatting.
- **`RetentionDays` is how long the card keeps history**, at `CardRetentionDays`
  (5 × `RetentionYearDays`, so 1825) — 365 flat per year, because a bound of this
  shape is `now - N days` and a calendar year would move it by a leap day and cut
  a term somebody bought short. Nothing computes it yet; it is a number pug
  renders (§13), so every deployment over-delivers by keeping everything. nil
  means **no bound at all** — never zero, exactly like an absent quota.
- **`Retired`** marks a card that may no longer be granted to an org not already
  on it. A retired card stays in the catalog forever so its existing customers
  keep resolving (§4.2), and `SetPlan` refuses it for anybody else
  (`ErrPlanRetired`). `NewService` refuses a catalog without exactly one live
  card, which is what makes `CurrentCard`'s panic unreachable in a wired service.
- **There is no list price.** A graduated card is a table, so no single number
  describes it: `Plan.price_cents` is `reserved 3` on the wire and the response
  carries the card's tiers instead (§7). `Cards()` and `CardBySlug` deep-copy the
  tier slice, so a caller cannot reprice the catalog by mutating what it was
  handed.
- **`Quote` is pug's arithmetic**, in `price.go`: `Price(card, events)` walks the
  bands, `PriceCustom(terms, events)` applies a deal's fee and rate. Each line
  rounds half-up on its own, so the lines a customer sees sum to the total rather
  than to a separately rounded figure. **Nothing invoices from it yet** — no
  production caller outside `Entitlement.Quote` exists; that arrives with
  usage-metered billing (§11).
- **The marketing site's pricing page is a second copy of this table**, hand-
  maintained in a different repo. Nothing enforces that they agree; a price
  change is two PRs, and this one is the one customers are actually held to.
- Adding a card is a Go const and nothing else. `plan_slug` carries **no** check
  constraint on the slug set: a list in the migration would be a second catalog to
  keep in lockstep, and the reprice workflow above — which mints a new slug —
  would fail against the stale copy with a raw SQLSTATE. `SetPlan` rejects a slug
  the catalog does not know, which is the same guard one layer up.

### 4.1 Negotiated deals

A deal we agree with one customer — "Acme, 5M events included, $400/mo plus $3
per million beyond, annual" — is **not** a catalog entry. It is the org's own
row, carrying the deal's terms plus overrides that layer over whichever card it
names:

| Field | Column | NULL means |
|---|---|---|
| Flat fee per period | `flat_fee_cents` | the deal charges no fee |
| Rate past the allowance | `rate_cents_per_million` | the deal charges no overage |
| Allowance | `included_events_override` | no allowance — charge from the first event |
| Retention | `retention_days_override` | use the card's number |
| Display name | `display_name_override` | use the card's name |

Term is `contract_ends_at`, and the paperwork reference lives in `note`. Nothing
about a bespoke deal needs a deploy, a catalog row or a join.

**The deal's price lives here.** This reverses the original design, which kept
amounts out of pug entirely (§14 records the change). The argument that won:
pug is what *computes* the amount. A graduated card is a table only pug holds, so
the arithmetic was always going to be pug's; a deal's fee and rate are not a stale
copy of the provider's number but the input that number is derived from. Without
them `Resolve` can say a deal exists and not what it costs, which makes a custom
org unpriceable — the one shape `Quote` must never return for a paying customer.
What the provider actually *charged* remains a separate, observed fact on
`billing_subscriptions.price_cents` ([`payments.md`](payments.md) §4); the two
are different questions and neither is a copy of the other.

**A deal must have a price; an allowance is optional.** `plan_slug = 'custom'`
requires a fee **or** a rate — `ErrCustomNeedsPrice` in `SetPlan`, mirrored by the
`custom_needs_price` check constraint (migration 021, which replaced the earlier
`custom_needs_quota`). A deal that charges nothing is not a deal. The allowance
moved the other way and became optional, because "$400/mo plus $3 per million
from the first event" is a real arrangement; `PriceCustom` handles a zero
allowance by simply having no free band.

> A flat fee with **no** rate is a fixed-price arrangement — one line, and no
> overage however much is sent. That is how a pre-usage plan is expressed now
> that the catalog sells only usage.

**A lapsed deal keeps neither its terms nor its allowance.** Both are gated on
`contract_ends_at`, and `Resolve`'s backstop then puts the org back on the current
card at `StatusFree`. The expensive mistake in either direction is symmetrical: a
lapsed deal that kept its allowance under-charges, and one that kept its fee
charges a customer who no longer has a contract. The one exception is a *live
custom subscription*, which its own contract date cannot expire — somebody is
still paying for it (§6).

**Retention is negotiable and optional.** An absent retention resolves as
*unlimited keeping*, which is what every org already gets and costs only storage,
so a deal that names no retention is stored as it is written.

**Where this stops being the right shape:** overrides describe *one* org. Sell the
same bespoke terms to twenty customers and there are twenty rows to keep in step —
at which point it has stopped being a deal and become a card, and belongs in the
catalog (§4) as a const, or in §11.2's table once one exists.

**A plan entitles a quota and nothing else.** There are no plan-gated features in
pug, so a deal cannot grant one. If features ever become plan-scoped, that is a
new decision, not an override column.

### 4.2 Repricing, and why cards are immutable

A Go catalog has no plan versions. Editing `usage-2026-09-1`'s first band from
4,000 to 5,000 cents changes what **every existing customer on that card** pays,
retroactively, the moment the deploy lands — a renegotiation of every live
agreement performed by a one-line diff, with no record that it happened and
nothing to compare against. This is the one failure mode that a rows-based
catalog handles for free (the archived design's `PLAN_STATUS_ARCHIVED` existed
for exactly this), so a Go catalog has to buy it back with a rule:

> **A card's `Currency`, `FreeEvents`, `Tiers` and `RetentionDays` are immutable
> once any org holds it.** Repricing mints a new slug — `usage-2026-09-2` — and
> marks the old one `Retired: true`. Nothing is ever deleted from the catalog.

The slug carries its own date for this reason: a card is a dated price list, and
naming it after the month it went on sale makes "which numbers did they buy?" a
question the slug answers by itself.

Retention most of all: cutting an allowance withholds something the customer has
not sent yet, while cutting retention is a promise to delete what they already
did.

Existing customers keep resolving against the slug they hold and are unaffected;
new ones get `CurrentCard()`, which is the newest card not retired. Grandfathering
is then the default rather than something an operator has to remember, and moving
a customer onto new terms becomes what it should be — a deliberate `pug billing
set`, recorded in the history (§5.1) with a note about who agreed to it.

What stays editable: `DisplayName`, because renaming "Usage" to "Standard"
changes nothing anyone bought. What this costs: a catalog that only grows, and
slugs that carry a date. Both are cheap next to a silent reprice.

## 5. Storage

Migration `019_create_billing_entitlements.sql`: the entitlement itself, and the
history behind it (§5.1). Migration `021_add_billing_deal_money.sql` added the
deal's two money columns to both tables and swapped `custom_needs_quota` for
`custom_needs_price` (§4.1). The DDL below is the shape after 021. The entitlement is a 1:1 extension of `orgs`, so the
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
  display_name_override varchar(150),
  -- What the deal charges every period regardless of volume. NULL or 0 means
  -- none: a deal with only a rate charges purely on usage. No currency column --
  -- the card catalog is USD throughout and an org cannot hold two (section 4).
  flat_fee_cents bigint
    constraint billing_entitlements_flat_fee_check check (flat_fee_cents >= 0),
  included_events_override bigint
    constraint billing_entitlements_override_check
      check (included_events_override is null or included_events_override > 0),
  note text not null default '',
  org_id char(20) primary key references orgs(id) on delete cascade,
  -- No slug check: the catalog is Go (catalog.go) and SetPlan already rejects an
  -- unknown slug. A list here would be a second catalog to migrate in lockstep.
  plan_slug varchar(50) not null,
  -- What the deal charges past included_events_override. NULL or 0 means no
  -- overage: a flat fee alone is a fixed-price arrangement.
  rate_cents_per_million bigint
    constraint billing_entitlements_rate_check check (rate_cents_per_million >= 0),
  -- How far back this org's events stay queryable. NULL means the plan's own
  -- retention. Nothing deletes on it (section 13).
  retention_days_override bigint
    constraint billing_entitlements_retention_check
      check (retention_days_override is null or retention_days_override > 0),
  trial_ends_at timestamptz,
  update_time timestamptz not null default now(),
  -- A custom deal has no card to fall back on, so nothing prices it unless the
  -- row says what it charges. An allowance is NOT required: a deal may charge
  -- from the first event. Enforced here rather than in the CLI: the row is what
  -- every read trusts, and `> 0` rather than not-null so a hand-written zero
  -- cannot pass as a price.
  constraint billing_entitlements_custom_needs_price
    check (plan_slug <> 'custom'
      or coalesce(flat_fee_cents, 0) > 0 or coalesce(rate_cents_per_million, 0) > 0)
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
- **The `*_override` columns** — a negotiated deal's quota, retention and name
  (§4.1). NULL means "use the plan's" in each case. `included_events_override`
  and `retention_days_override` are checked `> 0` because 0 would read as a quota
  or a retention of zero rather than as "no override", and because "unlimited" is
  deliberately not expressible here. A zero retention would be the worse of the
  two: it is the one value that could ever be read as "delete everything".
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
  display_name_override varchar(150),
  included_events_override bigint,
  note text not null default '',
  -- No FK: the history outlives the row, and an org's terms are still the
  -- answer to a question after the org is gone.
  org_id char(20) not null,
  plan_slug varchar(50),
  retention_days_override bigint,
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

`entitlement.Resolve(orgCreateTime, rec, sub, now, billingEnabled)` is a pure
function returning the resolved `Entitlement` — slug, display name, currency,
status, whichever of `Card`/`Terms` prices the org, `IncludedEvents`,
`RetentionDays`, trial/contract dates and the period bounds. No I/O, so the whole
rule set is unit-testable without a container. `orgCreateTime` is an argument
rather than something the package looks up because it is load-bearing twice: it is
the trial clock, and it is the default quota anchor (§6.1). `sub` is the org's
live subscription, or nil ([`payments.md`](payments.md)).

The period bounds are computed first and are independent of every branch below: a
plan changes what an org may send, never when its month turns over.

**Status** (`resolveStatus`) — what state the org is in, independently of what
prices it:

1. **Billing disabled** (§9) → `FREE`, no card, no terms, `IncludedEvents` nil.
   A self-hosted install has no quota at all, so no banner can fire even if a
   client forgets to check the flag. The switch fails *open* on the number,
   which is safe precisely because the number enforces nothing.
2. **A live subscription** → `ACTIVE`. Not gated on the contract: that date bounds
   an operator's grant, not something somebody is paying for.
3. **A pin that is present and not lapsed** → `ACTIVE` — `custom`, or a card slug
   the catalog still **resolves**. A slug that merely looks like a card is not
   enough: an org pinned to a card the catalog dropped is unpriceable, and calling
   it active would claim a paid subscription nothing can price.
4. **`trial_ends_at` (or the org's age) in the future, not lapsed** → `TRIALING`.
5. **Otherwise** → `FREE`.

Since free and trial are no longer plans (§4), "they are on a paid tier" and
"they pinned something" became the same question — which is why status has one
function and pricing has another.

**Pricing** (`resolveCard`) — most specific first: a live subscription's card
outranks a deal on the row, which outranks an operator's grant.

1. **A subscription naming a card** → that card. A live subscription must not be
   overridden by a lapsed deal or by a slug the catalog dropped.
2. **A `custom` row** → its `Terms` (§4.1), with the allowance gated on the
   contract.
3. **A row pinning a card** → that card; an unknown slug resolves to *nothing*
   rather than to the current card, because resolving it would price a customer
   on terms nobody sold them.
4. **Otherwise** → `CurrentCard()`.

**A granted plan outranks a live trial date** (3 before 4 in status), so a
customer who converted mid-trial can never be demoted by a stale timestamp.
`SetPlan` also clears `trial_ends_at` when it grants a non-floor plan, so in
practice the two rarely coexist — the ordering is what makes the resolver's answer
independent of whether that write happened.

Then each present override replaces the corresponding field of what resolved
(§4.1) — allowance, retention and display name — so a deal survives a catalog
reprice untouched.

Finally a **backstop**: a `custom` row that reached the end entitling the org to
nothing — no fee, no rate and no allowance — falls to the current card at `FREE`.
That is a deal against nothing: a lapsed contract (whose terms `applyOverrides`
has just cleared), or a row predating `custom_needs_price`. Both halves of the
test are needed, because a deal charging from the first event has no allowance and
must still stand (§4.1).

The overrides are gated on the **contract**, not on the resolved slug: a lapsed
`contract_ends_at` drops them, and a row with no contract date keeps them however
the plan resolves. So a comped free-tier bump is not wiped by the org still being
in its trial (§10) — and, the same way, an open-ended deal's allowance rides onto
a card the org later self-serve buys. `applyOverrides` does not compare
`plan_slug` to the card in force.

The one exception to the contract gate runs the other way: a live **custom**
subscription keeps its overrides past `contract_ends_at`, because that date bounds
an operator's grant and must not strip the quota of a deal somebody is being
charged for.

Whether a plan-in-force gate *should* exist is open. Nothing is enforced on a
quota, so the cost of the gap is a wrong number on a page, and adding the gate
would drop a live deal's quota the moment the org bought a cheaper tier.

An **unknown `plan_slug`** — only reachable if a slug is removed from Go while
rows still point at it — resolves to `IncludedEvents` nil (no quota). `Resolve`
stays pure, so the log happens in `Service.GetEntitlement`, which has a ctx to
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
| `GetBillingStatus` | `OrgGated(ResourceBilling, ActionRead)` | `billing_enabled`, plan (slug, display name, price cents, currency), derived status, `included_events`, `retention_days`, `trial_ends_at`, `contract_ends_at`, `period_start`, `period_end` |

The plan fields are the **resolved** ones — overrides already applied (§4.1), so
a client never reconstructs a deal from a base plan plus patches. `note` and the
history never cross the wire; both are operator data.

`currency` is always present alongside any amount — a card's tier rates, a deal's
fee and rate — and a client must format from the pair rather than assuming two
decimal places (§4).

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
  no such companion field. A client consuming it from `../app` must check presence
  rather than truthiness: `0` is a real value.
- **There is no `price_cents`.** `Plan.price_cents` is `reserved 3` on both `Plan`
  and `PlanOption`: a graduated card is a table, not a number (§4). The response
  carries `rate_card` (a card's `free_events` + tiers) or `custom_terms` (a deal's
  fee, rate and allowance) instead — exactly one while billing is on and the slug
  resolves, and **neither** when nothing prices the org. The number was reserved
  rather than repurposed because a field number is the only identity a field has:
  reusing 3 would have let an old client decode a tier table as an `Int64Value`
  price, silently, since both are length-delimited.
  `CustomTerms`' three values are `Int64Value` wrappers for the same reason as
  above — absent means the deal has no such term, and a deal with a rate and no
  fee charges from the first event.
- **`retention_days` is absent-able on the same terms**, and is the same
  `Int64Value` wrapper for the same reason — absent is *no bound*, and "0 days of
  history" is the one thing it must never say. It states what the plan promises,
  not what has been deleted: nothing prunes on it (§13), so a client must not
  render it as "data older than this is gone".
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
pug billing set  <org-id> --plan <slug> --actor <who> [--events N]
                          [--flat-fee CENTS] [--rate-per-million CENTS]
                          [--retention-days N]
                          [--name "Acme Enterprise"] [--anchor-day 17]
                          [--until 2027-01-01] [--note "$400/mo, INV-123"]
                          [--provider-product prod_2f9k...]
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
- **A `set` to a FLOOR plan does not cancel a running trial.** The trial is the
  org's age (§6), derived identically whether a row exists, so recording an
  anchor day or a note on a three-day-old org leaves it trialing. A `set` to a
  *granted* plan does end the trial state — the plan resolves ahead of the trial
  date (§6), and `applyChange` clears `trial_ends_at` with it.

A negotiated deal (§4.1) is one `set`:

```shell
pug billing set o_2f9k --plan custom --events 5000000 --retention-days 2555 \
                       --flat-fee 40000 --rate-per-million 300 \
                       --name "Acme Enterprise" --actor "praveen/INV-123" \
                       --until 2027-01-01 --note "INV-123"
```

`--flat-fee` and `--rate-per-million` are what the deal charges — $400 a period
plus $3 per million past the 5M allowance, above. **`--plan custom` is refused
without at least one of them** (`ErrCustomNeedsPrice`, §4.1): a deal that charges
nothing is not a deal. `--events` is optional, so a deal may charge from the first
event. Both are cents, as integers, because the card catalog is a single currency
and a decimal here would be a rounding argument nobody wants during a renewal.
`--note` is for the paperwork reference, not the amount — the amount is now a
column a query can read.

`--events`, `--flat-fee`, `--rate-per-million`, `--retention-days`, `--name` and
`--anchor-day` write the override
columns; omitting one on a re-`set` leaves the stored value alone, and passing
the empty value (`--events 0`, `--flat-fee 0`, `--rate-per-million 0`,
`--retention-days 0`, `--name ""`, `--anchor-day 0`) clears it back to the
card's. Leaving them alone is the right
default because the common re-`set` is a renewal — a new `--until` on terms that
have not changed — and a flag that silently reverted a customer's negotiated
quota to a catalog number would be the most expensive bug this CLI could have.

Guards on `set`, all refusing rather than guessing: an unknown slug; the `trial`
slug (`extend-trial` is its only writer); a **retired** tier (§4) unless the org
already holds it, so it cannot be handed to someone new by autocomplete; `custom`
left with no quota. That last one is checked against the *merged* row, not the
flags, so a re-`set` on a deal that already carries an override needs no
`--events`. `--events`, `--flat-fee` and `--rate-per-million` refuse a negative rather than
reading it as a clear, and `--anchor-day` is range-checked in both the CLI and the
service.
`extend-trial` refuses an org holding a granted plan — including a slug the
catalog no longer knows, which resolves free without ever consulting a trial date
— since the write would store a date that changes nothing.

Every write appends to the history (§5.1) in the same transaction, attributed to
the `--actor` it was given. `show --history` prints the org's
changes newest-first, which — with `note` — is what a refund or renewal argument
is actually settled from.

This CLI is why the slice is usable rather than decorative — without a writer,
the table is dead and the RPC only ever reports the derived floors. If it should
be smaller, the honest floor is `show` + `set`; `extend-trial` and `clear` are
conveniences over the same two columns.

Every command prints the same report — the org, the switch, `RESOLVED`, `STORED`
and optionally `HISTORY` — so a write is confirmed by the state it produced
rather than by an "ok". Three things about that report are load-bearing:

- **An absent value prints `(none)`, never `0`.** Absent `included_events` means
  NO quota (§7); a zero would state a billing figure the deployment never claimed.
  The same holds for a deal's fee and rate, which print only when the deal has
  them — a deal charging purely on usage shows no fee line rather than `$0.00`.
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
| `PUG_BILLING_ENABLED` | `false` | The single switch. Off ⇒ `billing_enabled=false` and no quota anywhere (§6). Set it on every pod of a billed deployment. |

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
- **Retention** — each tier resolves its own ladder value; a negotiated
  `retention_days_override` wins and lapses with its contract; an unknown slug
  and a disabled deployment both report *no bound* rather than the floor's year,
  which is the same fail-open direction the quota takes.
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
  stored, inserts successfully, which is the test that catches a slug outgrowing
  `varchar(50)` or being rejected by the service; a `custom` row without
  `included_events_override` is rejected by the database, not merely by the CLI.
  There is deliberately no `plan_slug` check constraint (§5).
- **History** — every mutating CLI path appends exactly one snapshot in the same
  transaction, `clear` included; a failed write appends nothing; the history
  survives its org being deleted. The last of those is the one a foreign key
  would quietly break, so it is a test rather than a comment.
- **Catalog immutability** (§4.2) — a golden test pins every tier's
  `Currency`, `FreeEvents`, `Tiers` and `RetentionDays`, so editing a live card fails CI
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
2. **Checkout** (payments provider) — designed in [`payments.md`](payments.md),
   which supersedes the sketch below where the two differ: products, checkout sessions, a signed
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
3. **Retention enforcement** — the prune that makes §4's number more than a
   promise. It is deliberately its own slice because it is the first thing in
   this subsystem that would *destroy* customer data, and three things have to be
   decided in it rather than discovered after: what a downgrade or a lapse does
   to history already stored (proposed: a grace period and a notice, never an
   immediate delete — §12), whether the bound is a ClickHouse TTL or a job that
   can be halted, and what stops a resolution bug from deleting on a wrong
   number. Until it exists pug keeps everything, which over-delivers on every
   tier.
4. **Payment ledger**: invoices, recorded manual payments, refunds and
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
- **A lapse or a downgrade shortens retention retroactively.** A 10-year deal
  that ends resolves to the free floor's 365 days the same instant its quota
  drops, so the *stated* bound moves across years of already-stored history at
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
fifty times its quota, and the only signal is a banner shown to the person with
the least reason to act on it. That is a ClickHouse cost exposure and an abuse
vector, not a billing gap — the fix belongs with the meter, which already sweeps
every project on a schedule, and is noted in [`usage.md`](usage.md). It does not
need a quota to be useful: "any org over N events/day" catches the same traffic
and works for custom deals too.

**Nothing enforces retention.** The tier's `RetentionDays` is a plan term with no
prune behind it: no ClickHouse TTL, no delete job, no query clamp, and no path
that reads the number for anything but rendering it. Invariant 1 keeps billing
out of *ingestion*; deletion is the larger promise, so it is §11.3's own slice
rather than a switch flipped here. The consequence is stated plainly: a customer
on a 1-year tier can still query year-old data, and pug pays to store it.

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
- **Overrides are gated on the contract date** (§6), so an expired 5M deal cannot
  keep its 5M for good. They are *not* gated on the resolved slug: an open-ended
  deal's numbers ride onto whatever tier is in force.
- **The `GetUsage` RPC now answers `NotFound` for an unknown org.** It previously
  reported a metered zero for any id, because the period came from the clock
  alone and no lookup could fail. Resolving an anchor requires the org row, so a
  missing one is now a real error — same `ORG_NOT_FOUND` reason the orgs service
  uses.
- **`included_events` shipped as an `Int64Value` wrapper** (and `price_cents`
  did too, while it existed), not the
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
  is the only reader, and nothing serves it over the network. (`ListPlans` later
  arrived with checkout — payments §17.)
- **`show` prints the org's display name, which no billing query returns.** The
  CLI reads `orgs` directly for it. An operator pastes an id and is about to
  write to it; "Acme Inc" beside `o_2f9k` is the only thing in the report that
  catches the wrong org before the write, and it is cheap.
- **`--until ""` clears the contract end.** §8 lists the empty value of every
  other override as its clear but not this one, which left a deal's end date
  unremovable without a `set` back to a floor plan.
- **The CLI validates `--anchor-day` and `--events` itself**, as §8 says for the
  anchor day and does not for the quota. Both are also checked in the service,
  which is what a second caller would hit; the CLI's copy exists so the message
  names the flag rather than the column.

- **The plan catalog became a graduated rate card catalog** (§4). `free`,
  `starter`, `growth` and `scale` — fixed monthly prices with fixed quotas — are
  gone, replaced by one `RateCard` priced in usage bands, with `free`, `trial` and
  `custom` demoted from plans to *states*. Pug sells usage, so a tier list was
  describing a product it no longer had. `Plan.price_cents` went `reserved` in the
  same move (§7).
- **A deal's money is stored in pug** (§4.1), reversing the "no structured amount
  per org" decision in §3 and [`payments.md`](payments.md) §4. The original
  argument was that a copy of the provider's number is a second authority that
  goes stale; what it missed is that pug is the thing that *computes* the amount.
  A graduated card is a table only pug holds, so `Quote` was always going to be
  pug's arithmetic — which makes a deal's `flat_fee_cents` and
  `rate_cents_per_million` (migration 021) the **inputs** to that computation, not
  a copy of its result. Without them `Resolve` can say a deal exists and not what
  it costs. `note` remains where the paperwork reference goes; it is no longer
  where the amount goes. What the provider actually charged stays on
  `billing_subscriptions.price_cents`, which is an observed fact and a different
  question.
- **`custom_needs_quota` became `custom_needs_price`** (§4.1, migration 021). A
  deal now requires a fee or a rate and no longer requires an allowance, because
  "$400/mo plus $3 per million from the first event" is a real arrangement while a
  deal that charges nothing is not one.
