# Billing — usage-based pricing

> **Status: implemented 2026-09-12, unverified against Dodo.** Written
> 2026-09-11. This replaces the flat monthly tiers of [`billing.md`](billing.md)
> §4 and the product-per-tier checkout of [`payments.md`](payments.md) §5/§13
> with metered, tiered pricing billed monthly in arrears through a Dodo
> **on-demand subscription**. Billing is off in production, so there was no org,
> plan or subscription to migrate: the old tiers are deleted, not retired.
>
> Everything below is built except the decisions §19 leaves to you (the
> placeholder prices are what shipped, pinned by the golden test) — code, tests,
> migration 021 and the `cron-billing-invoice` image. **Three things are still
> open**, and all three are §18.2's test-mode run:
>
> 1. **The mandate-only checkout's shape** (§7.1). `FetchCheckoutOutcome` now
>    walks session → payment → subscription and, when a $0 authorization's
>    payment names no subscription, falls back to listing the customer's
>    subscriptions and matching the `checkout_ref`. Which route Dodo actually
>    takes is untested against the real API.
> 2. **`PATCH /subscriptions/{id}` accepting `next_billing_date`** on an
>    on-demand subscription (§7.3 layer 1). Implemented as
>    `SetNextBillingDate`; if Dodo refuses it the pass reports the call as
>    unreadable and the cancellation write-off grows from a day to a month.
> 3. **What the checkout page shows a buyer** for a mandate-only session.
>
> `billing.md`, `payments.md` and `usage.md` still describe the tier model and
> are now **stale where they disagree with this file**. The fold-in stays where
> §18.5 put it: after your sign-off, not before.

Today an org buys a tier with a fixed quota and Dodo charges a fixed price on
its own schedule. After this, an org **authorizes a card once**, pug **counts**
what it sent (it already does), **prices** it on a versioned rate card, and
**charges the amount it computed** through Dodo. The provider stops owning the
price; pug stops pretending it has no opinion on money.

---

## 1. Scope

**In:** a rate card (per-100k-event blocks, graduated tiers, a removable free
allowance, 5-year retention), rate-card versioning with automatic
grandfathering, custom deals as a flat fee and/or a flat block rate, a card
mandate via Dodo's on-demand subscriptions, an invoice ledger, an hourly
invoicing pass that closes periods and charges, pug-owned retries and dunning,
a running estimate in the dashboard, and the operator CLI for all of it.

**Out:** enforcement of any kind (invariant 1 stands — see §10 for what
"removing the free tier" can and cannot mean), multi-currency, per-seat
pricing, prepaid credits (§15), annual commitments, refunds initiated from pug
(Dodo's dashboard does that; pug only records the outcome), org deletion
(still no path), and the marketing site's pricing page (a second copy of the
rate card in another repo, as today).

**Unchanged:** the meter (`usage.md`), the anniversary window, the entitlement
history, the webhook inbox and its CAS, the provider seam, `ConfirmCheckout`,
the portal, USD-only.

## 2. Structural invariants

1. **Ingestion never consults billing.** No event is rejected, throttled or
   dropped for a bill, an unpaid invoice or a missing card. Nothing on the
   ingestion path imports `internal/core/billing`.
2. **Pug owns the quota AND the price; Dodo owns the mandate, tax, the receipt
   and the payout.** This inverts payments.md §4 ("pug stores no money"), and
   deliberately: an on-demand charge carries an amount, and the only system
   that can compute it is the one holding the count. There is no second
   authority to disagree with any more — Dodo's products carry no price pug
   charges.
3. **No charge without a durable invoice row first.** The invoice is the
   intent; the API call is the action; the row commits before the call. A
   process that dies between the two leaves a row that says so.
4. **One pricing function.** The dashboard estimate, the invoicing pass, the
   CLI preview and the tests all call `Price(card, events)`. A customer can
   never be shown one number and charged another.
5. **Every state is derived or recorded, never both.** Entitlement status
   stays derived from the clock (billing.md invariant 3); invoice status is a
   recorded state machine because a charge is an event, not a comparison.
6. **One writer per row.** Operator → `billing_entitlements`. Invoicing pass
   and payment webhooks → `billing_invoices`. Subscription webhooks, reconcile
   and confirm → `billing_subscriptions`. Nothing crosses.
7. **A retry never guesses.** With no idempotency key at the provider (§8.4),
   a charge whose outcome is unknown is resolved by *reading* Dodo, never by
   charging again on a hunch.

## 3. The pricing model

### 3.1 Blocks and rounding

The unit of billing is a **block** of 100,000 events. An org's period usage —
`uniqExact(event_id)` across all its projects over its anniversary window,
exactly the number `usage.md` already produces — rounds to the **nearest**
block:

```
blocks = (events + 50_000) / 100_000      -- integer division; a tie rounds up
```

| events | blocks | note |
|---|---|---|
| 149,999 | 1 | inside the free block |
| 150,000 | 2 | the tie rounds up: first paid block |
| 2,340,000 | 23 | |
| 2,350,000 | 24 | tie |

Nearest rather than ceiling is the customer-friendly reading you asked for; the
cost is a $5 cliff between 149,999 and 150,000 events, which is where "paid
from 200k" in the brief comes from — 150k rounds to 200k. Documented rather
than hidden.

### 3.2 The rate card

Graduated (marginal) tiers over cumulative blocks. Placeholder prices:

| blocks | events | per block |
|---|---|---|
| 1 | first 100k | **free** |
| 2 – 10 | 100k – 1M | $5 |
| 11 – 50 | 1M – 5M | $4 |
| 51 + | > 5M | $3 |

Worked: 2,340,000 events → 23 blocks → block 1 free, blocks 2–10 = 9 × $5 =
$45, blocks 11–23 = 13 × $4 = $52 → **$97.00**. 7,000,000 events → 70 blocks →
$45 + 40 × $4 + 20 × $3 = **$265.00**.

**Graduated, not volume.** Volume pricing (every block at the rate of the tier
the total lands in) has a cliff in the wrong direction: 10 blocks = 9 × $5 =
$45, 11 blocks = 10 × $4 = $40 — sending more costs less. Graduated is
monotonic, and it is what "from 1M–5M it'll be $4 per 100k" says. Volume is a
one-line change in `Price` if you want it (§17.1).

**Retention is 5 years on every card and every deal:** `RetentionDays =
5 * RetentionYearDays` (1,825). Still a rendered promise, still unenforced
(billing.md §13); the per-org override stays for the deal that wants more.

### 3.3 In Go

```go
type RateCard struct {
    Slug, DisplayName string
    Currency          string   // USD
    BlockEvents       int64    // 100_000
    FreeBlocks        int64    // 1; 0 removes the free tier (§10)
    Tiers             []Tier   // ascending; the last has UpToBlock 0 = unbounded
    RetentionDays     int64
    Retired           bool
}
type Tier struct{ UpToBlock, CentsPerBlock int64 }

// Price is the one pricing function (invariant 4). Pure.
func Price(card RateCard, events int64) Quote   // Quote{Blocks, Lines, TotalCents}
func PriceCustom(t CustomTerms, events int64) Quote
```

`catalog` becomes `[]RateCard`, newest last, first slug `usage-2026-09`. The
existing `TestCatalogIsPinned` golden test carries over: every field of a card
is immutable once any org holds it. `starter`/`growth`/`scale` are **removed**,
not retired — nobody holds them in production.

## 4. Versioning and grandfathering

Same mechanism billing.md §4.2 already has, applied to cards:

- **A price change mints a new slug** (`usage-2027-01`) and sets
  `Retired: true` on the old one. Nothing is edited in place, nothing deleted.
- **An org is pinned to the card in force when its mandate activated.** Pug
  writes the current non-retired slug into `billing_checkout_sessions` when it
  opens the mandate checkout; the delivery attributed by that ref carries it
  onto `billing_subscriptions.plan_slug`. Reconcile re-reads keep the stored
  slug (the same "a product only has to resolve to grant" rule the
  cancellation path uses today). From then on the org resolves against that
  slug until an operator moves it.
- **An org with no mandate always sees the current card.** It agreed to
  nothing, so there is nothing to grandfather; its free allowance is whatever
  the current card says.
- **The operator overrides both** with `pug billing set --plan usage-2026-09`:
  a card slug on the entitlement row wins over the subscription's, which is how
  a customer is grandfathered by hand, moved onto new terms deliberately, or
  given a retired card as a favour. `SetPlan` keeps refusing a retired slug for
  an org that does not already hold it.

Pinning at **mandate activation** rather than at org creation is a decision
(§19.3): the mandate is the moment the customer agreed to a price, and it is
already the row that outlives everything else.

## 5. Custom deals

A negotiated deal is `plan_slug = 'custom'` plus terms on the org's own row —
the same shape as today, with money in it:

| Term | Column | Meaning |
|---|---|---|
| Flat fee | `flat_fee_cents` | charged every period regardless of usage; NULL = none |
| Block rate | `block_rate_cents` | per 100k over the allowance; NULL = usage beyond the allowance is not charged |
| Allowance | `included_events_override` | events before the block rate applies; NULL = none. Must be a whole number of blocks |
| Retention, name, term, note | as today | |

```
amount = flat_fee + max(0, blocks(events) − allowance/100k) × block_rate
```

So a flat-only deal is a fee with no rate (effectively unlimited for the fee),
a rate-only deal is a flat $/100k with an optional allowance, and "Acme: $400 a
month, 5M included, $3 per 100k after" is all three: 7,200,000 events → 72 −
50 = 22 × $3 = $66 → **$466.00**. No tiers, as asked.

**A custom deal needs at least one of `flat_fee_cents` / `block_rate_cents`**,
enforced by a check constraint, replacing today's `custom_needs_quota`. An
allowance alone is a free tier nobody agreed to.

**The price lives in pug now — the reversal of payments.md §4.** That section
ruled it out because Dodo held the real price and a copy would drift. Under
on-demand billing Dodo holds no price at all: the number pug sends is the
number charged, so the row is the only authority. `--note` stays for the
paperwork; it is no longer where the amount hides.

**`provider_product_id` goes.** Every org, custom or not, authorizes against
the one mandate product (§7.1); there is nothing per deal to create in Dodo.
That deletes the product-paste step, the staged-product attribution branch and
the payment-link fallback in one move.

**A lapsed contract falls to the current card, not to free.** The deal ended;
the customer is still a customer with a live mandate, and the card is what
anyone without a deal pays. (Today a lapsed deal falls to the free floor; that
was right when the floor was the only alternative to a tier.)

## 6. Resolution

`Resolve` keeps its shape — a pure function over the org's age, the row, the
mandate and the clock — and its output changes from "a tier" to "a card or a
deal, and whether it can be charged":

```
1. billing disabled           → FREE, no card, no allowance, no bound (unchanged)
2. live mandate?              → chargeable; card = row.plan_slug if set, else
                                sub.plan_slug (pinned), else current card
3. row is custom, not lapsed  → terms from the row (chargeable only with a mandate)
4. no mandate                 → current card; FREE, or TRIALING inside the trial
5. any open failed invoice    → status PAST_DUE (derived from the ledger, §8.6)
```

`Entitlement` gains `Card *RateCard`, `Terms *CustomTerms`, `Chargeable bool`,
`NextChargeAt time.Time`. `IncludedEvents` keeps its wire meaning — events this
period before charges begin — and becomes the card's free allowance or the
deal's allowance, so the existing usage meter renders "80k of 100k free" for a
free org with no change. `PriceCents` is absent for every card (there is no
flat price), which the dashboard already handles for `custom`.

**The trial survives as a no-charge window.** Days inside the trial are
clipped out of the billable window (§8.1). With a free block it is moot; if
the free tier is removed (§10) it is the evaluation period. `TrialDays` stays
14, `extend-trial` is unchanged.

**The anchor question closes.** billing.md §6.1 left open whether to align the
provider's charge date to pug's anniversary or the reverse. On-demand means
pug picks the charge instant, so the invoice date *is* the anniversary and
`anchor_day` stays the operator-only override it is. No `anchor_day` is ever
written at checkout.

## 7. The mandate

### 7.1 One product, authorized once

Dodo's on-demand subscription is a stored payment method with a subscription
id pug can charge arbitrary amounts against. Checkout changes in one place:

- **One Dodo product** — a monthly subscription product named for the receipt
  ("Pug usage billing"), tax-exclusive so pug's amount is the pre-tax line,
  configured as `PUG_DODO_MANDATE_PRODUCT`. It replaces the three
  `PUG_DODO_PRODUCT_<TIER>` keys. Its stored price is never charged.
- **`subscription_data.on_demand.mandate_only = true`** on the checkout
  session: authorizes the card, charges nothing. `show_on_demand_tag` so the
  page says what it is. Everything else — metadata `org_id` + `checkout_ref`,
  USD lock, new-customer-per-checkout, theme — stays as today.
- **`CreateCheckoutSession(plan_slug)`** accepts the current card's slug or
  `custom`; the product is the same either way, the slug is what gets pinned.
  `ListPlans` returns the current card (one `PlanOption` carrying a
  `rate_card`) plus `custom` for an org whose row has terms.

**VERIFY in test mode before building on it:** whether a mandate-only checkout
session exposes a `payment_id` (today `FetchCheckoutOutcome` walks session →
payment → subscription, and a $0 authorization may leave the session with no
payment). If not, `ConfirmCheckout` needs a second route to the subscription —
`Subscriptions.List(customer_id)` filtered on the `checkout_ref` metadata — and
the webhook stays the authority. Also verify what the checkout page shows a
buyer for a mandate-only session.

### 7.2 What changes at the webhook

- **`on_demand` must be true.** A recurring subscription reaching pug would be
  charged by Dodo on its schedule *and* by pug's invoices. Rejected at the
  boundary like a foreign currency: stored, marked processed, not applied,
  reported by reconcile.
- **`price_cents` mirrors 0** (`recurring_pre_tax_amount` of a mandate). The
  column stays; the money is on invoices now.
- **`past_due` joins the status map** (Dodo has it as a status; pug maps only
  `on_hold` today). Both are live.
- Attribution loses its staged-product branch (§5); `checkout_ref` then
  customer id, as today.

### 7.3 Cancellation is where usage billing loses money

The charge comes *after* the usage, so the mandate has to outlive the last
period. Three layers, cheapest first:

1. **Pug pins Dodo's `next_billing_date`** to the org's next charge instant
   (`period_end + grace + 1d`, §8.1) on activation and after every invoice.
   Dodo's portal hides "cancel now" for on-demand subscriptions and schedules
   cancellation for `next_billing_date`, so a customer cancelling in the
   portal keeps the mandate alive until just after pug has charged the final
   period. **VERIFY** that `PATCH /subscriptions/{id}` accepts
   `next_billing_date` on an on-demand subscription; the SDK exposes it, the
   docs are silent.
2. **A scheduled cancellation triggers an early close.** On a delivery
   carrying `cancel_at_next_billing_date = true`, the next pass invoices the
   period to date and charges while the mandate is live. Whatever is sent
   between then and the cancellation is the write-off — a day under layer 1,
   up to a month without it.
3. **`RemovePaymentMethod` RPC (admin)** — pug's own cancellation, which does
   the steps in the right order: close the period at today, charge, then
   `PATCH status=cancelled`. Offered in the dashboard beside "Manage billing";
   the portal path stays possible and is covered by 1 and 2.

A cancelled mandate cannot be charged ("no longer chargeable"), so a final
invoice that finds one is `uncollectible` (§8.5) and a reconcile finding, not a
retry loop.

`subscription.update_payment_method` (a new card via the portal) re-opens the
org's `failed` and `uncollectible` invoices for another attempt (§8.5).

## 8. Invoices

### 8.1 Closing a period

An hourly pass (`pug cron billing-invoice`, §11) closes each org's period once
it is safe to price, and prices it:

- **When:** `period_end + grace` has passed, where `grace =
  PUG_USAGE_RESCAN_DAYS` (2 days, the meter's own trailing window). After that
  instant the trailing rescan no longer re-reads the period's last day, so the
  count is as final as the meter makes it. The pass additionally requires the
  org's most recent `usage_computed_at` to be **at or after** `period_end +
  grace`: a stalled meter delays an invoice, never mis-bills one.
- **What it sums:** `usage_daily` over the **billable window** —
  `[max(period_start, mandate_day, trial_end_day), min(period_end,
  cancellation_day))` at day grain — with a new `SumUsageDaily` read. The
  first invoice of a mandate covers only days from the day the card was
  added; the last covers only days before it was removed; trial days are
  never billed. `usage_periods` is untouched: it stays the dashboard's live
  number, and the invoice stores its own count, which is the bill's.
- **Which orgs:** those with a mandate live at any point in the period, and
  those on a custom plan. A free org gets no invoice row; its over-allowance
  usage is a reconcile finding (§10), not a bill.
- **What it writes:** one `billing_invoices` row, `open` — or `waived` when
  the amount is under `MinChargeCents` (50¢; a zero-usage month on a rate-only
  deal, or a free-allowance month), which is recorded and never sent to Dodo.
  The row snapshots the card or terms it was priced on (`pricing jsonb`), so a
  later catalog edit cannot change what an invoice says it charged.

### 8.2 Storage (migration 021)

```text
billing_invoices
  id                    char(20) primary key
  org_id                char(20) not null references orgs(id) on delete cascade
  provider              text                   -- null for waived (nothing to charge)
  provider_sub_id       text                   -- the mandate charged
  plan_slug             varchar(50) not null   -- card slug or 'custom'
  pricing               jsonb not null         -- the RateCard or CustomTerms used
  period_start          timestamptz not null
  period_end            timestamptz not null
  billed_from           date not null          -- the clipped window, section 8.1
  billed_to             date not null
  event_count           bigint not null
  blocks                bigint not null
  lines                 jsonb not null         -- [{description, blocks, cents_per_block, amount_cents}]
  amount_cents          bigint not null check (amount_cents >= 0)
  currency              varchar(3) not null
  status                text not null          -- section 8.3
  attempts              int not null default 0
  next_attempt_at       timestamptz
  last_error_code       text not null default ''
  last_error_message    text not null default ''   -- merchant-facing; never on the wire
  provider_payment_id   text                   -- set once; unique (provider, provider_payment_id)
  provider_invoice_url  text                   -- Dodo's receipt, for ListInvoices
  usage_computed_at     timestamptz not null   -- the meter stamp the count came from
  paid_at, failed_at    timestamptz
  create_time, update_time

  unique (org_id, period_start)                -- one invoice per period, ever

billing_invoice_events                          -- append-only, like the entitlement history
  id, invoice_id, at, from_status, to_status, actor, detail
```

`amount_cents` and `currency` are honest names while §3 of payments.md (USD
only) holds, as `price_cents` is today. No FK from invoices to
`billing_subscriptions`: a mandate row can be replaced across a provider
cutover and the invoice must keep saying which subscription id was charged.
Invoices are never pruned — they are the ledger billing.md §11.4 promised.

### 8.3 The state machine

```
open ──charge──▶ charging ──payment_id──▶ charged ──webhook/poll──▶ paid
  ▲                 │                                 │
  │   (ambiguous)   │ settle by listing (8.4)          └──▶ failed ──soft, attempts<4──▶ open (next_attempt_at)
  └─────────────────┘                                        └──hard, or 4th──▶ uncollectible ──new card──▶ open
open, amount < min ──▶ waived
any non-terminal  ──operator──▶ void
paid ──refund.succeeded──▶ refunded
```

Terminal: `paid`, `refunded`, `waived`, `void`, and `uncollectible` until a new
payment method arrives. Every transition appends to `billing_invoice_events`
with an actor — the pass, a webhook id, or the operator.

### 8.4 Charging, without an idempotency key

Dodo's charge endpoint takes no idempotency key (verified against SDK
v1.115.0: no such request option exists), so a call that times out after Dodo
created the payment is indistinguishable from one that never arrived.

**The SDK's own transport retry is therefore disabled on this one call**
(`option.WithMaxRetries(0)`). Its default is 2 retries on a nil response —
which includes a connection reset and the client timeout firing — and on
408/409/429/5xx, so leaving it on would re-POST the charge and take the money
up to three times while pug saw a single error. Reads keep their retries.

The pass therefore:

1. Sets `charging` and **commits** (invariant 3).
2. Calls `POST /subscriptions/{sub}/charge` with `product_price =
   amount_cents`, `product_currency = USD`, `product_description = "Pug — 2.34M
   events, 17 Aug – 17 Sep 2026"`, and **explicit metadata** `{org_id,
   invoice_id, period_start}` — the charge inherits the subscription's metadata
   only when none is passed, and pug needs the invoice id on every payment
   webhook.
3. On a response: `charged` + `provider_payment_id`. On a definitive refusal —
   a 4xx the **card or mandate** refused — `failed`, the error stored. A 401,
   403, 408, 409 or 429 is pug's problem, not the card's, and is treated as
   ambiguous: dunning a customer because a key was rotated or a rate limit was
   hit would write them off over 17 days. On anything ambiguous — timeout, 5xx,
   connection reset — **leaves the row in `charging`**.
4. **Settles `charging` rows older than a few minutes by reading**:
   `Payments.List(subscription_id, created_at_gte = invoice.create_time)` and
   look for `metadata.invoice_id`. The list has no promised order and a retry
   after a decline shares the invoice id with the attempt that failed, so the
   match is the **newest succeeded** payment, not the first one seen. Found →
   adopt the payment id (`charged`); not found → `open`, and the next tick
   charges again. **The reopen counts an attempt**, so that cycle is bounded by
   `max_charge_attempts` and ends in `uncollectible` rather than re-charging
   every hour forever — each lap POSTs a charge that may really take money. A
   retry after a *read* is the only retry there is (invariant 7).
5. On marking an invoice `paid`, the payment's `total_amount`/`currency` are
   checked against what pug billed. A mismatch does not block the transition —
   the money moved — but it is recorded, because nothing else would notice a
   charge that took the wrong amount.

`payment.succeeded` / `payment.failed` deliveries — stored and ignored today —
now settle `charged` invoices by `metadata.invoice_id`, through the existing
inbox. As with `ConfirmCheckout`, the webhook is not the only route: the pass
also polls `Payments.Get` for `charged` rows older than an hour, so a
deployment with no reachable webhook URL still learns whether it was paid.

### 8.5 Retries and dunning are pug's

Dodo states it does not retry failed on-demand charges. Pug owns the policy,
and follows Dodo's own recommendation:

| Outcome | Action |
|---|---|
| soft decline (`INSUFFICIENT_FUNDS`, `PROCESSING_ERROR`, `NETWORK_ERROR`, `TRY_AGAIN_LATER`) | `failed`; retry at +3d, +7d, +7d after the first attempt (4 attempts over 17 days), then `uncollectible` |
| hard decline (`STOLEN_CARD`, `LOST_CARD`, `DO_NOT_HONOR`, `FRAUDULENT`, …) | `uncollectible` immediately; retrying damages authorization rates |
| a new payment method (`subscription.update_payment_method`) | every `failed`/`uncollectible` invoice → `open`, `next_attempt_at = now` |
| mandate cancelled / expired | open invoices → `uncollectible`; a finding |

`last_error_message` is merchant-facing and never crosses the wire; the
dashboard gets a generic "payment failed" and a link to the portal.

**Nothing is enforced.** A `PAST_DUE` org keeps sending events and keeps
being invoiced; the cost of dunning is a banner and an email, never a degraded
product (payments.md §11, unchanged). Whether pug should ever cancel a mandate
itself after the fourth failure is a decision (§19.8); the recommendation is
no — a cancelled mandate is one that can never be retried.

### 8.6 What the ledger derives

- `BillingStatus` gains `PAST_DUE` (additive enum value): any `failed` or
  `uncollectible` invoice. Derived at read time from the ledger, so it clears
  the instant a retry succeeds, with no sweep.
- `NextChargeAt` = current `period_end + grace`, what the dashboard shows as
  "next charge".
- The running estimate (§9) is `Price(card, current period usage)` — the same
  function, over the live `usage_periods` number, with its three-state
  freshness carried through.

## 9. RPC surface

Additive on the wire (the app is live; nothing is removed or renumbered):

| RPC | Spec | Change |
|---|---|---|
| `GetBillingStatus` | viewer floor, unchanged | `+ rate_card` (block_events, free_blocks, tiers), `+ custom_terms` (flat_fee_cents, block_rate_cents, included_events), `+ chargeable`, `+ next_charge_at`; `status` may be `PAST_DUE` |
| `GetUpcomingInvoice` | **new**, `OrgGated(ResourceBilling, ActionRead)` | the current period priced so far: `event_count`, `blocks`, `lines`, `amount_cents`, `usage_computed_at`, `counted` — the same three-state freshness as `GetUsage`, so "unknown" never renders as $0 |
| `ListInvoices` | **new**, admin-only via `ResourceInvoice` + `ActionRead` granted to admin | closed periods newest-first: amounts, status, `provider_invoice_url` (Dodo's receipt), dates. Never `last_error_message` |
| `CreateCheckoutSession` | unchanged shape | opens a mandate-only checkout; `plan_slug` must be the current card or `custom` |
| `RemovePaymentMethod` | **new**, admin (`ActionCreate` alongside checkout) | §7.3 layer 3 |
| `ListPlans` | unchanged spec | `PlanOption + rate_card`; `price_cents` absent |
| `ConfirmCheckout`, `CreatePortalSession` | unchanged | |

`GetUpcomingInvoice` on the viewer floor and `ListInvoices` admin-only is a
split worth stating: the estimate is the same information as the usage meter
everyone sees, priced; the history carries receipts with a company's billing
details on them (billing.md §11.4's argument). Decision §19.6.

`GetUpcomingInvoice` folds a usage number into a billing RPC, which billing.md
§7 refused to do. The refusal was about *duplicating* the freshness contract;
this RPC restates it with the identical two fields, and the alternative —
pricing in the browser — duplicates `Price`, which is the worse copy to have.

## 10. Removing the free tier

The rate card makes it a number: a new card version with `FreeBlocks: 0`
charges new orgs from their first block, and every pinned org keeps its free
block until an operator moves it. That is the whole pricing change.

What it does **not** do is make anyone pay. Invariant 1 keeps ingestion blind
to billing, so an org with no mandate sends events and is never invoiced,
today and after. "No free tier" therefore also needs a gate that is *not* on
the ingestion path — **mandate-required at signup**: a future
`PUG_BILLING_MANDATE_REQUIRED` under which `projects.Create` and
`CreateApiKey` answer `FailedPrecondition(BILLING_MANDATE_REQUIRED)` for an
org past its trial with no live mandate. RPC-level, reversible, and the trial
becomes the evaluation window. Not built in this slice; the design keeps
`Chargeable` on the resolved entitlement so the gate is one check when it
comes.

Until then the reconcile pass reports **unbilled usage**: a closed period over
the current card's free allowance for an org with no mandate. That is the
number that says how much the free tier is costing.

## 11. The invoicing pass

`cmd/cron/billing-invoice` (image `cron-billing-invoice`), shaped exactly like
the two passes that exist: one-shot, `cron.WithLock` on its own
`JobBillingInvoice` key, `passTimeout`, non-zero exit on failure, lock
contention exits 0, a root span before config. **Hourly**: anniversaries are
spread across the month and retries are dated to the hour. Under the lock, in
order:

1. **Close** every period that is due and safe (§8.1).
2. **Charge** every `open` invoice with `next_attempt_at ≤ now` and a live
   mandate (§8.4).
3. **Settle** `charging` rows by listing, `charged` rows by polling (§8.4).
4. **Pin** `next_billing_date` on mandates whose next charge moved (§7.3).
5. **Report**: periods held back by a stale meter, uncollectible invoices,
   invoices whose mandate is gone, unbilled usage (§10) — counted and logged,
   exported as `billing.invoice_pass_total{outcome}` and
   `billing.invoices_total{status}`, alongside the reconcile counters.

A separate binary rather than a stage of `billing-reconcile`: reconcile is
"nothing auto-fixed" and read-mostly, this one moves money, and the two want
different cadences and different alerts. The cost is a fourth CronJob in
gitops. Decision §19.9.

**Every failure that is a person's to fix is a finding, not an exit code**
(a declined card, a cancelled mandate, a period the meter has not reached).
The pass exits non-zero when it could not read or write — Postgres, or **any**
provider call (`Unreadable`, matching the reconcile pass: one org charging
successfully says nothing about the ones that did not) — or when it left a
charge unresolved (`Ambiguous`), since that is money in an unknown state. So a
red CronJob means the pass itself is broken, not that a customer's card is.

## 12. Operator CLI

```shell
pug billing show <org-id> [--history] [--invoices]
pug billing set  <org-id> --plan usage-2026-09|custom --actor <who>
                          [--flat-fee 40000] [--block-rate 300] [--events 5000000]
                          [--retention-days N] [--name ...] [--anchor-day N]
                          [--until 2027-01-01] [--note ...]
pug billing extend-trial <org-id> --days 30 --actor <who>
pug billing clear <org-id> --actor <who>
pug billing preview <org-id> --events 2340000          # Price() on the org's card or terms
pug billing invoice void  <invoice-id> --actor <who> --note "..."
pug billing invoice retry <invoice-id> --actor <who>   # uncollectible → open, now
```

- `--flat-fee` and `--block-rate` are USD cents; omitted keeps, `0` clears,
  the same merge rule as every other flag. `--provider-product` is gone.
- `set --plan custom` refuses a row with neither fee nor rate, and an
  `--events` that is not a whole number of blocks.
- `show` prints `RESOLVED` with the card or terms and the next charge date,
  `STORED` with the money columns, and `--invoices` the ledger newest-first
  with status and attempts — the operator's view of a dunning conversation.
- `preview` is how a deal is sanity-checked before it is set: it is `Price`,
  which is what the customer will be charged.
- `void` is the only way to stop a wrong charge before it happens or to record
  that a paid one was refunded in Dodo's dashboard; it never calls the
  provider. `retry` is for the customer who fixed their card by phone.

Every write still appends to `billing_entitlement_history`, which gains the
two money columns; invoice writes append to `billing_invoice_events`.

## 13. Configuration

| Var | Default | Meaning |
|---|---|---|
| `PUG_BILLING_ENABLED` | `false` | unchanged |
| `PUG_BILLING_PROVIDER` | `""` | unchanged |
| `PUG_DODO_API_KEY`, `PUG_DODO_ENVIRONMENT`, `PUG_DODO_WEBHOOK_SECRET` | | unchanged |
| `PUG_DODO_MANDATE_PRODUCT` | — | the one on-demand product every org authorizes against. **Replaces** `PUG_DODO_PRODUCT_STARTER/GROWTH/SCALE`. Absent ⇒ not purchasable, as a missing tier key is today |
| `PUG_USAGE_RESCAN_DAYS` | `2` | now also the invoicing grace (§8.1). Read by the invoice pass too, so the two cannot drift |
| `MinChargeCents` | 50 | a Go const; under it an invoice is `waived` |

## 14. Migration 021

- `billing_entitlements`: `+ flat_fee_cents bigint check (>= 0)`, `+
  block_rate_cents bigint check (>= 0)`, `− provider_product_id`;
  `custom_needs_quota` replaced by `custom_needs_price` (§5). The history table
  mirrors all three, and its deletion constraint is rewritten again.
- `billing_subscriptions`: `+ on_demand boolean not null`.
- `billing_checkout_sessions`: `+ plan_slug varchar(50) not null` (§4).
- `billing_invoices`, `billing_invoice_events` (§8.2).
- `cron.LockBillingInvoice` joins the advisory-lock iota.

No data migration and no backfill: production has billing off and every
billing table empty. The three tier product keys are deleted from gitops; the
Dodo products behind them are retired in Dodo's dashboard by hand.

## 15. Alternatives considered

You asked for a better strategy if there is one. The vehicle you named is the
right one; here is why the others lose, and what I would change around it.

| Strategy | Why not |
|---|---|
| **Dodo usage meters** (`/events/ingest`, price per unit + free threshold) | Per-unit only — no tiers, graduated or otherwise. And "events timestamped more than 1 hour in the past are rejected": pug's meter is a revising batch that re-reads two days back, which can never be replayed into a one-hour window. Dodo would also need every event, not a daily count. |
| **Dodo credit-based billing with overage** (v1.86) | Credits per cycle + overage at one rate. Models "100k free then $X" but not three tiers, and pug still pushes usage to Dodo. |
| **Recurring subscription + add-on quantity** (the discarded overage design) | Gated on the unverified `do_not_bill` semantics, couples the charge to Dodo's `next_billing_date`, and graduated tiers need one add-on per tier. On-demand strictly dominates for pure usage pricing. |
| **Prepaid wallet / credits** (`customer_balance_config`) | Cash up front, no dunning — but it is "buy blocks", not "pay for what you used", and Dodo's wallet semantics are undocumented. Worth revisiting as the *entry ticket* if the free tier goes (§10): "prepay $10 to start" is a friendlier gate than "add a card". |

And around the on-demand design:

1. **Graduated over volume** (§3.2) — the brief reads as graduated; volume
   would let a customer pay less by sending more.
2. **Pin the card at mandate time, not org creation** (§4) — the mandate is
   the agreement; an org that never paid has nothing to grandfather.
3. **One Dodo product, ever** (§7.1) — no product per tier, none per deal,
   nothing to paste. Custom deals become a CLI command and nothing else.
4. **Clip the first and last invoice to the mandate's days** (§8.1) — a
   customer who added a card on the 28th is not billed for the 27 days before
   it. Cheap because usage is day-grain already.
5. **Pug pins Dodo's `next_billing_date`** (§7.3) — the one trick that makes
   the portal's cancellation land after the final charge instead of before it.
   Verify it first; it is the difference between a one-day and a one-month
   write-off on every cancellation.
6. **A customer-set spend alert, later** — not a cap. Usage billing without a
   number the customer controls is what people fear about it; the meter
   already has the data and the estimate already exists. A notification, on
   the read side of invariant 1.
7. **Let `ListPlans` be the pricing page's source** — the marketing site is
   another repo and will drift; if it can read the rate card from the public
   API, the two copies collapse to one.

## 16. Testing

`internal/core/billing` keeps `TestMain` and no `t.Parallel()`; the provider
stays a fake.

- **`Price`** — table-driven, written first: every row of §3.1/§3.2, tie
  rounding, tier boundaries at 10 and 50 blocks, `FreeBlocks: 0`, an unbounded
  last tier, and the golden pin on every card's fields. `PriceCustom`: fee
  only, rate only, both, allowance larger than usage, an allowance that is not
  whole blocks refused upstream.
- **Grandfathering** — a retired card keeps resolving for its holder; a new
  mandate pins the current card; the entitlement slug wins over the
  subscription's; a lapsed custom contract falls to the current card.
- **Period close** — clipped windows for a mid-period mandate, a mid-period
  cancellation, and a trial; a stale meter holds the close; `waived` under the
  minimum; exactly one invoice per `(org, period_start)` under two concurrent
  passes (the unique index, not the lock, is the guard).
- **The charge state machine** (Postgres) — every edge of §8.3; the ambiguous
  outcome: a fake that creates the payment and then errors, settled by listing
  and never charged twice; a fake that errors without creating one, re-opened
  and charged once; soft vs hard declines and the retry dates; a new payment
  method re-opening `uncollectible`.
- **Webhooks** — `payment.succeeded`/`.failed` settle by `metadata.invoice_id`
  and ignore a payment carrying none; a non-on-demand subscription is rejected
  at the boundary; `past_due` maps live.
- **RPC freshness** — `GetUpcomingInvoice` carries the three usage states
  through and never renders an unknown count as $0.
- **Authz** — the registry and policy tests fail until `ResourceInvoice` and
  the two new procedures are entered; nothing else to write.

## 17. Known imprecision

1. **Events arriving more than `grace` days after their `occur_time` are never
   billed.** They land in `usage_daily` (the dashboard sees them) but the
   invoice has closed. Errs in the customer's favour and is dwarfed by 100k
   rounding.
2. **An erasure after invoicing credits nothing.** The invoice froze its count
   (§8.1). A credit note is a refund in Dodo's dashboard and `invoice void`.
3. **The estimate and the invoice can differ** — the estimate prices the live
   `usage_periods` number, the invoice sums the clipped window at close. Same
   function, different inputs, documented on the page ("estimate").
4. **Rounding to the nearest block is a $5 cliff on one event** (§3.1).
5. **Tax is Dodo's.** Pug shows pre-tax amounts; the receipt shows tax.
6. **A charge at `period_end + 2d` means the invoice date is not the
   anniversary but two days after it.** The period is; the charge lags by the
   grace, and `next_charge_at` says so.
7. **Cancellation write-off** (§7.3): at least a day of usage, a month if the
   `next_billing_date` pin turns out unsupported.
8. The meter's own imprecisions (`usage.md` §8) are inherited unchanged.

## 18. Rollout

1. Migration 021, then the server, `cron-billing-reconcile` and the new
   `cron-billing-invoice` images. The ordering constraint is the usual one for
   a new column read by a live path: 021 before the images.
2. In Dodo **test**: create the mandate product; verify the three marked
   VERIFY items (§7.1 session shape, §7.3 `next_billing_date`, the checkout
   page's rendering of a $0 authorization); run a mandate → close → charge →
   `payment.succeeded` → portal cancel cycle end to end.
3. gitops: `PUG_DODO_MANDATE_PRODUCT`, drop the three tier keys, schedule the
   invoice CronJob hourly.
4. Flip `PUG_BILLING_ENABLED` when the dashboard side (`../app`: rate card,
   estimate, invoices, "Add payment method" replacing "Upgrade") has landed.
5. `CLAUDE.md` pointers and the fold-in of this document into the three docs
   come after your sign-off, not before.

## 19. Decisions

Each has a recommendation; the plan above assumes it.

1. **Graduated tiers** (not volume). Recommend yes.
2. **Ties round up** (150,000 events → 2 blocks). Recommend yes; it is what
   "nearest" needs to be deterministic and it matches the brief's "from 200k".
3. **Pin the rate card at mandate activation** (not org creation). Recommend
   yes.
4. **Clip the first and last invoice to the mandate's days**, at day grain.
   Recommend yes. Alternative: bill the whole period the card was added in.
5. **Keep the 14-day trial as a no-charge window.** Recommend yes; it costs
   nothing while a free block exists and is the evaluation period if it goes.
6. **`GetUpcomingInvoice` on the viewer floor, `ListInvoices` admin-only.**
   Recommend yes.
7. **Waive invoices under 50¢** rather than carry them forward. Recommend yes;
   the smallest non-zero card invoice is $5.
8. **Dunning: 4 attempts over 17 days, then `uncollectible` + banner + email;
   pug never cancels the mandate itself.** Recommend yes.
9. **A separate `cron-billing-invoice` binary** rather than a stage of
   reconcile. Recommend yes, at the price of a fourth CronJob.
10. **A lapsed custom contract falls to the current card**, not to free.
    Recommend yes.
11. **Placeholder prices and bounds** — $5 / $4 / $3 at 1M and 5M, 100k free,
    5-year retention. Yours to change; the golden test pins whatever ships.
12. **`RemovePaymentMethod` in this slice** (§7.3 layer 3) or later. Recommend
    this slice: it is the one path where pug controls the order of charge and
    cancel.
