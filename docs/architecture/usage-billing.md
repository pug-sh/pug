# Billing — usage-based pricing

> **Status: design approved 2026-09-15, subject to §19's decisions and §18.2's
> test-mode checks; not yet built.** Written 2026-09-11,
> **revised 2026-09-15**: usage is now priced **per event** on graduated tiers
> (100k events free a month, then $40 per million, stepping down with volume)
> instead of per 100k-event block, and small invoices are handled with Dodo's
> per-transaction fee in view (§8.7) instead of a flat 50¢ waiver. §20 is the
> delta against the block model already built on `feat/usage-billing`, §21
> what the tier system loses, and §22 the order the code lands in. This
> replaces the flat monthly tiers of [`billing.md`](billing.md) §4 and the
> product-per-tier checkout of [`payments.md`](payments.md) §5/§13 with metered,
> tiered pricing billed monthly in arrears through a Dodo **on-demand
> subscription**. Billing is off in production, so there is no org, plan or
> subscription to migrate: the old tiers are deleted, not retired.
>
> Nothing here is on `main` yet. The code lands in follow-up PRs, one slice at a
> time, and this document is what they are reviewed against. §19 records each
> decision with the recommendation the plan assumes, and the prices in §3 are
> **placeholders** (§19.11) until they are set.
>
> **Some things cannot be settled from Dodo's docs** and need §18.2's test-mode
> run before the code that depends on them is trusted:
>
> 1. **The mandate-only checkout's shape** (§7.1) — whether a $0 authorization's
>    session names its subscription, or whether the customer's subscription list
>    has to be matched back on the `checkout_ref`.
> 2. **`PATCH /subscriptions/{id}` accepting `next_billing_date`** on an
>    on-demand subscription (§7.3 layer 1). If Dodo refuses it, the cancellation
>    write-off grows from a day to a month.
> 3. **What the checkout page shows a buyer** for a mandate-only session.
> 4. **What a small charge really costs** (§8.7): whether an on-demand charge
>    carries the 0.5% usage-billing surcharge, whether a declined attempt is
>    charged for, and whether an Indian card's USD mandate has a ceiling.
> 5. **How tax lands on an on-demand charge** (§7.1): the charge takes no tax
>    flag of its own, so that `product_price` is taxed on top under the
>    tax-exclusive product, and what a validated business tax ID does to that
>    tax.
>
> §10's `PUG_BILLING_MANDATE_REQUIRED` gate is designed here and deliberately
> left unbuilt.
>
> `billing.md`, `payments.md` and `usage.md` describe the tier model that is
> live today; they go stale where they disagree with this file only once it
> ships. The fold-in is §22's fifteenth commit, landing with the code.

Today an org buys a tier with a fixed quota and Dodo charges a fixed price on
its own schedule. After this, an org **authorizes a card once**, pug **counts**
what it sent (it already does), **prices** it on a versioned rate card, and
**charges the amount it computed** through Dodo. The provider stops owning the
price; pug stops pretending it has no opinion on money.

---

## 1. Scope

**In:** a rate card (per-event graduated tiers, a removable free allowance,
5-year retention), rate-card versioning with automatic grandfathering, custom
deals as a flat fee and/or a flat per-event rate, a card mandate via Dodo's
on-demand subscriptions, an invoice ledger, an hourly invoicing pass that
closes periods and charges, fee-aware handling of small invoices (§8.7),
pug-owned retries and dunning, a running estimate in the dashboard, and the
operator CLI for all of it.

**Out:** enforcement of any kind (invariant 1 stands — see §10 for what
"removing the free tier" can and cannot mean), multi-currency, per-seat
pricing, prepaid credits (§15), annual commitments, refunds initiated from pug
(Dodo's dashboard does that; pug only records the outcome), billing emails
(the invoice notice and the dunning mail are a later PR; the notice window they
announce is built here, §8.1), org deletion
(still no path), the marketing site's pricing page (a second copy of the
rate card in another repo, as today), and a second billable dimension — a
surcharge on identified users or stored profiles is a second count with its
own card, not a change to this one; the meter counts one thing.

**Unchanged:** the meter (`usage.md`) apart from its window catching up after an
outage (§8.1), the anniversary window, the entitlement history, the webhook
inbox and its CAS, the provider seam, `ConfirmCheckout`, the portal, USD-only.

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
4. **One pricing path.** The dashboard estimate, the invoicing pass, the CLI
   preview and the tests all price through `Price(card, events)` for a card and
   `PriceCustom(terms, events)` for a deal, chosen in one place from the
   resolved entitlement (§6). A customer can never be shown one number and
   charged another.
5. **Every state is derived or recorded, never both.** Entitlement status
   stays derived from the clock (billing.md invariant 3); invoice status is a
   recorded state machine because a charge is an event, not a comparison.
6. **Every invoice write is a guarded transition.** Operator →
   `billing_entitlements`. Subscription webhooks, reconcile and confirm →
   `billing_subscriptions`. `billing_invoices` has more than one writer — the
   invoicing pass, payment webhooks, `subscription.update_payment_method`,
   `RemovePaymentMethod` and the operator's `invoice void` and `retry` — and
   none of them holds the pass's lock, so every invoice transition is an update
   guarded on the status it leaves: the writer that loses a race changes nothing
   and calls no provider.
7. **A retry never guesses.** With no idempotency key at the provider (§8.4),
   a charge whose outcome is unknown is resolved by *reading* Dodo, never by
   charging again on a hunch.

## 3. The pricing model

### 3.1 Per event, exactly

The unit of billing is **one event**. An org's period usage —
`uniqExact(event_id)` across all its projects over its anniversary window,
exactly the number `usage.md` already produces — is priced as it is. Nothing
is rounded to a block, so there is no cliff anywhere on the curve: the
100,000th event is free and the 100,001st costs its rate and nothing more.

### 3.2 The rate card

Graduated (marginal) tiers over cumulative events. **Placeholder prices**
(§19.11): 100k events free a month, then $40 per million, each band about 30%
cheaper than the one before:

| events | per event | per million | cents/M (stored) |
|---|---|---|---|
| first 100k | **free** | — | 0 |
| 100k – 2M | $0.000040 | $40 | 4,000 |
| 2M – 15M | $0.000028 | $28 | 2,800 |
| 15M – 50M | $0.000020 | $20 | 2,000 |
| 50M – 100M | $0.000014 | $14 | 1,400 |
| 100M – 250M | $0.000010 | $10 | 1,000 |
| 250M + | $0.000007 | $7 | 700 |

A bound is inclusive: "100k – 2M" is events 100,001 through 2,000,000.

**Prices exclude tax.** Every price in this document — a card's rates, a deal's
fee and rate, the estimate, an invoice — is pre-tax USD, and every surface that
shows one says "excluding tax". Dodo, as merchant of record, adds the tax the
customer's billing address requires when it charges, and a business can give
its tax ID at checkout (§7.1). Tax is never folded into a rate, so a rate is
the same pre-tax amount in every country.

Worked: 200,000 events → 100k free, 100,000 × $40/M → **$4.00**. 1,500,000 →
1,400,000 × $40/M → **$56.00**. 2,340,000 → 1,900,000 × $40/M = $76.00, 340,000
× $28/M = $9.52 → **$85.52**. 20,000,000 → $76.00 + 13,000,000 × $28/M =
$364.00 + 5,000,000 × $20/M = $100.00 → **$540.00**.

**Graduated, not volume.** Volume pricing (every event at the rate of the tier
the total lands in) has a cliff in the wrong direction: 2,000,000 events →
1,900,000 × $40/M = $76.00; 2,000,001 → 1,900,001 × $28/M = $53.20 — sending
more costs less. Graduated is monotonic. Volume would be a one-line change in
`Price` (§15, item 1).

**Retention is 5 years on every card and every deal:** `CardRetentionDays =
5 * RetentionYearDays` (1,825). Still a rendered promise, still unenforced
(billing.md §13); the per-org override stays for the deal that wants more.

### 3.3 Arithmetic

A rate is stored as **cents per million events** (`CentsPerMillion`), an
integer: one cent per million is $0.00000001 per event, so even a rate like
$0.00002744 an event is a whole number, 2,744. Fractions of a cent per event
never exist in the code.

```
line_microcents = events_in_tier × cents_per_million        -- exact
line_cents      = (line_microcents + 500_000) / 1_000_000   -- half up, per line
total_cents     = Σ line_cents
```

Each line is rounded to whole cents on its own and the total is the sum of
the lines, so what the customer sees adds up; the cost is at most half a cent
per line. Rounding the exact total once would be a hair more accurate and
would show lines that do not sum. A line under half a cent — 100,124
events is 124 × $40/M = 0.496¢ — rounds to $0.00, which §8.7 turns into
`waived`. `int64` holds it: a trillion events in one tier at 10,000 cents/M
is 10¹⁶ microcents.

### 3.4 In Go

```go
type RateCard struct {
    Slug, DisplayName string
    Currency          string   // USD
    FreeEvents        int64    // 100_000; 0 removes the free tier (§10)
    Tiers             []Tier   // ascending; the last has UpToEvents 0 = unbounded
    RetentionDays     int64
    Retired           bool
}
type Tier struct{ UpToEvents, CentsPerMillion int64 }

// Price and PriceCustom are the only pricing functions (invariant 4). Pure.
func Price(card RateCard, events int64) Quote   // Quote{Events, Lines, TotalCents}
func PriceCustom(t CustomTerms, events int64) Quote
```

`catalog` becomes `[]RateCard`, newest last, first slug `usage-2026-09-1`, whose
trailing number counts that month's cards so two prices minted in one month each
get a slug of their own. The
existing `TestCatalogIsPinned` golden test carries over: a card's `Currency`,
`FreeEvents`, `Tiers` and `RetentionDays` are immutable once any org holds it,
and `Retired` is pinned too, so retiring a card is a deliberate edit to the
golden. `starter`/`growth`/`scale` are **removed**, not retired — nobody holds
them in production.

## 4. Versioning and grandfathering

Same mechanism billing.md §4.2 already has, applied to cards:

- **A price change mints a new slug** (`usage-2027-01-1`) and sets
  `Retired: true` on the old one. Nothing is edited in place, nothing deleted.
- **An org is pinned to the card it was shown when it opened its mandate
  checkout.** Pug writes the current non-retired slug into
  `billing_checkout_sessions` when it opens that checkout; the delivery
  attributed by that ref carries it onto `billing_subscriptions.plan_slug`, even
  if the card retired while the checkout was open, since that is the price the
  buyer saw. Reconcile re-reads keep the stored slug; a delivery's product
  decides nothing (§21). From then on the org resolves against that slug until
  an operator moves it.
- **An org with no mandate always sees the current card.** It agreed to
  nothing, so there is nothing to grandfather; its free allowance is whatever
  the current card says.
- **The operator overrides both** with `pug billing set --plan usage-2026-09-1`:
  a card slug on the entitlement row wins over the subscription's, which is how
  a customer is grandfathered by hand, moved onto new terms deliberately, or
  given a retired card as a favour. `SetPlan` keeps refusing a retired slug for
  an org that does not already hold it.

Pinning at the **mandate checkout** rather than at org creation is a decision
(§19.3): the checkout is where the customer agreed to a price, and the mandate
it creates is already the row that outlives everything else.

## 5. Custom deals

A negotiated deal is `plan_slug = 'custom'` plus terms on the org's own row —
the same shape as today, with money in it:

| Term | Column | Meaning |
|---|---|---|
| Flat fee | `flat_fee_cents` | charged every period regardless of usage; NULL = none |
| Rate | `rate_cents_per_million` | per event over the allowance, as cents per million (§3.3); NULL = usage beyond the allowance is not charged |
| Allowance | `included_events_override` | events before the rate applies; NULL = none. Any whole number of events |
| Effective from | `terms_effective_at` | stamped when a deal starts or a lapsed one renews; the first day the deal is invoiced from |
| Retention, name, term, note | as today | |

```
amount = flat_fee + (max(0, events − allowance) × rate + 500_000) / 1_000_000   -- half up, as §3.3
```

So a flat-only deal is a fee with no rate (effectively unlimited for the fee),
a rate-only deal is a flat $/million with an optional allowance, and "Acme:
$400 a month, 5M included, $30 per million after" is all three: 7,200,000
events → 2,200,000 × $30/M = $66 → **$466.00**. A deal has no tiers, and its
fee and rate are pre-tax like the card's (§3.2).

**A custom deal needs at least one of `flat_fee_cents` /
`rate_cents_per_million`**, enforced by a check constraint, replacing today's
`custom_needs_quota`. An allowance alone is a free tier nobody agreed to.

**A deal is invoiced from `terms_effective_at`, not from the row's birth.** The
row usually predates the deal — `extend-trial` writes one months earlier — so
dating the terms by `create_time` would let recording a deal in September
retroactively invoice every elapsed period at the new rate. Only renewing a
**lapsed** deal restamps it, whatever the money says, or the gap it ran through
would be invoiced at the renewal's rate — and the lapsed deal's days not yet
invoiced leave the deal with the gap (§17.10). Any other write, a reprice or a
note on a lapsed deal included, keeps the stamp, so the terms price every day
not yet invoiced; restamping would move those days off the deal.

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
deal, and whether it can be charged". Rule 5 and `NextChargeAt` sit outside it:
`GetEntitlement` layers them on after it returns, both read off the ledger in
Postgres. Rules 1–4 are first match; rule 5 applies over whichever matched.

```
1. billing disabled           → FREE, no card, no allowance, no bound (unchanged)
2. row is custom, not lapsed  → terms from the row; chargeable only with a live mandate
3. live mandate?              → chargeable; card = the first of row.plan_slug,
                                sub.plan_slug (pinned) that names a card, else
                                the current card (a lapsed `custom` names none)
4. no mandate                 → current card; FREE, or TRIALING inside the trial
5. failed or uncollectible    → status PAST_DUE (any such invoice, from the ledger, §8.6)
```

`Entitlement` gains `Card *RateCard`, `Terms *CustomTerms`, `Chargeable bool`,
`NextChargeAt time.Time`; with billing on, exactly one of `Card` and `Terms` is
set. `IncludedEvents` keeps its wire meaning — events this
period before charges begin — and becomes the card's `FreeEvents` or the deal's
allowance, so the existing usage meter renders "80k of 100k free" for a free org
with no change; an `included_events_override` on a card pin replaces
`FreeEvents` outright, with no block arithmetic in between. `PriceCents` goes
from the entitlement and `plan.price_cents` from the wire, since no card has a
flat price (§21).

**The trial survives as a no-charge window.** Days inside the trial are
clipped out of the billable window (§8.1). With 100k free it is moot; if
the free tier is removed (§10) it is the evaluation period. `TrialDays` stays
14, `extend-trial` is unchanged. Recording a deal ends a running trial at that
instant rather than erasing the date, so a close still skips the days it covered.

**The anchor question closes.** billing.md §6.1 left open whether to align the
provider's charge date to pug's anniversary or the reverse. On-demand means
pug picks the charge instant, so the invoice follows the anniversary by
exactly the grace (§17.6), and `anchor_day` stays the operator-only override
it is; moving it is safe, because a close never bills a day twice (§8.1),
though the stub period it leaves is an invoice of its own (§17.11). No
`anchor_day` is ever written at checkout.

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
  page says what it is, and `allow_tax_id` left on (Dodo's default) so a
  business can give its VAT or GST number. Everything else — metadata `org_id`
  + `checkout_ref`, USD lock, new-customer-per-checkout, theme — stays as
  today.
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
buyer for a mandate-only session, and how tax lands on a charge:
`POST /subscriptions/{id}/charge` takes no tax flag, so confirm `product_price`
is taxed on top under the tax-exclusive product, and what a validated tax ID
does to that tax.

### 7.2 What changes at the webhook

- **`on_demand` must be true.** A recurring subscription reaching pug would be
  charged by Dodo on its schedule *and* by pug's invoices. Rejected at the
  boundary like a foreign currency: stored, marked processed, not applied,
  reported by reconcile.
- **`tax_inclusive` must be false.** A tax-inclusive mandate would carve the
  tax out of pug's amount instead of adding it on top (§3.2), and nothing
  downstream would notice. Rejected at the boundary the same way.
- **The subscription's price is not mirrored.** A mandate's
  `recurring_pre_tax_amount` is never charged, so `price_cents` goes (§21); the
  money is on invoices now.
- **`past_due` is named in the status map** (Dodo has it as a status; today it
  is live only by falling through verbatim, beside the mapped `on_hold`). Both
  are live — Dodo's on-demand guide is explicit that `on_hold` does not stop a
  charge, so a failed invoice retries against it.
- Attribution loses its staged-product branch (§5); `checkout_ref` then
  customer id, as today. The plan slug comes from the checkout session or the
  stored subscription, never from the product.

### 7.3 Cancellation is where usage billing loses money

The charge comes *after* the usage, so the mandate has to outlive the last
period. Three layers, cheapest first:

1. **Pug pins Dodo's `next_billing_date`** a day after the org's
   `NextChargeAt` (`period_end + grace + ChargeNoticeDays + 1d`, §8.6) on
   activation and after every invoice; the day absorbs the meter's hourly
   stamp and the pass's own hour (§8.1). Dodo's portal hides "cancel now" for on-demand subscriptions
   and schedules cancellation for `next_billing_date`, so a customer
   cancelling in the portal keeps the mandate alive until just after pug has
   charged the final period. **VERIFY** that `PATCH /subscriptions/{id}`
   accepts `next_billing_date` on an on-demand subscription; the SDK exposes
   it, the docs are silent.
2. **A scheduled cancellation triggers an early close — but only when the
   cancellation lands first.** On a delivery carrying
   `cancel_at_next_billing_date = true`, the pass invoices the period to date
   and charges while the mandate is live **if** the mandate ends before
   `period_end + grace`. When layer 1 has pinned the cancellation past that
   instant the natural close already catches a live mandate, and closing early
   would only split one charge, and its fee (§8.7), into two. An **unread** end
   date waits for the natural close for the same reason; that close sweeps
   (§8.7) if the mandate went. Whatever is sent between the early close
   and the cancellation is the write-off — a day under layer 1, up to a month
   without it. An invoice already waiting out its notice window (§8.1) is
   charged at once when a cancellation lands that ends the mandate before the
   invoice's charge date.
3. **`RemovePaymentMethod` RPC (admin)** — pug's own cancellation, which does
   the steps in the right order: close the period at `now − grace`, charge it
   and any invoice still in its notice window at once, then `PATCH
   status=cancelled`. The days the meter has not finalized are the
   write-off. It **refuses to cancel unless the charge settled**
   (`BILLING_FINAL_PERIOD_UNSETTLED`): a decline, an ambiguous charge or a held
   meter leaves the mandate live, because cancelling first turns a retryable
   decline into a write-off. "Settled" is the provider's answer about the
   payment, not its acceptance of the charge — the RPC polls the payment it just
   made, and any of the org's invoices still `open`, `charging`, `charged` or
   `failed` refuses, including on a retry of the RPC that closes nothing and
   charges nothing. So does a previous period not yet closed: within `grace` of
   an anniversary there is nothing to close yet, and cancelling would leave
   that whole period to a gone mandate. The final close is a sweep (§8.7): it
   folds any carried balance into the last invoice, and a last invoice `waived`
   under the sweep floor counts as settled. A refused cancellation leaves the
   rest of the period to the natural close, which starts where this one ended
   (§8.1). Offered in the dashboard beside "Manage billing"; the portal path
   stays possible and is covered by 1 and 2.

A cancelled mandate cannot be charged ("no longer chargeable"), so a final
invoice that finds one is `uncollectible` (§8.5) and reported by the invoicing
pass as `mandate_gone` (§11), not a retry loop.

`subscription.update_payment_method` (a new card via the portal) re-opens the
org's `failed` and `uncollectible` invoices for another attempt (§8.5).

## 8. Invoices

### 8.1 Closing a period

The invoicing pass (`cmd/cron/billing-invoice`, §11) closes each org's period once
it is safe to price, and prices it:

- **When:** `period_end + grace` has passed, where `grace =
  PUG_USAGE_RESCAN_DAYS` (2 days, the meter's own trailing window). After that
  instant the trailing rescan no longer re-reads the period's last day, so the
  count is as final as the meter makes it. The pass additionally requires the
  org's most recent `usage_computed_at` to be **at or after** `period_end +
  grace`. That stamp alone proves little — the trailing rescan of any pass after
  that instant starts after the period — so the meter changes in one place: its
  window never starts later than its last successful pass's own window. A meter
  back from an outage re-reads every day that pass had not finalized before it
  stamps, so a stalled meter delays an invoice, never mis-bills one. A pass
  closes the three newest due periods; every older one still unbilled is
  written off as a `waived` invoice with a `dropped` event, whatever it cost,
  which moves `last_billed_to` past it and fails that pass (§11).
- **What it sums:** `usage_daily` over the **billable window**, at day grain,
  with a new `SumUsageDaily` read — for a card org `[max(period_start,
  last_billed_to, mandate_day, trial_end_day), min(period_end,
  cancellation_day))`, for a deal `[max(period_start, last_billed_to,
  terms_effective_day, trial_end_day), min(period_end, contract_end_day))`; a
  deal org's days outside its deal bill as a card org's do, on the card. `last_billed_to` is where
  the org's latest invoice ended, so no day is billed twice however the period
  moves: an anchor change, an early close, a card removed and added back. The
  first invoice of a mandate covers only days from the day the card was added;
  the last covers only days before it was removed; a deal bills from the day
  its terms took effect, card or not (§5, §19.15); trial days are never
  billed; an empty window writes no row. A period that saw more than one
  mandate closes once, summing only the days a mandate was live and priced on
  the latest, so it gets one allowance and the days between a removed card and
  its replacement are not billed, though its `[billed_from, billed_to)` spans
  them. A subscription with an unrecognized status is not live, here as
  everywhere, so none of its days are billed. `usage_periods` is untouched: it
  stays the dashboard's live number, and the invoice stores its own count, which
  is the bill's.
- **Which orgs:** those with a mandate live at any point in the period, and
  those on a custom plan, card or not (§19.15). A free org gets no invoice row;
  its over-allowance usage is reported by the invoicing pass as unbilled usage
  (§10), not a bill.
- **What it writes:** one `billing_invoices` row. Its own usage priced is
  `usage_cents`; any balance carried from the org's `deferred` invoices is
  `carried_cents`; `amount_cents` is the sum, and §8.7 decides its status —
  `open` at or above `DeferUnderCents`, `deferred` below it, `waived` when
  there is nothing to bill. The row snapshots the card or terms it was priced
  on (`pricing jsonb`), so a later catalog edit cannot change what an invoice
  says it charged.
- **When it is charged:** not in the pass that closes it. An `open` row's
  first charge is dated `ChargeNoticeDays` (§13) after the close, in
  `next_attempt_at`, so the invoice can be seen — and voided (§12) — before the
  card is charged; the billing email that announces it is a later PR. Two cases
  charge at once: a close forced by a mandate that is going (§7.3), and a
  deal's backlog when its first card arrives, whose notice ran while it waited
  (§19.15).

### 8.2 Storage (migration 021)

```text
billing_invoices
  id                    char(20) primary key
  org_id                char(20) not null      -- no FK, like the entitlement history: the ledger outlives the org
  provider              text                   -- stamped with provider_sub_id at each charge; null before the first
  provider_sub_id       text                   -- the mandate the latest attempt charged
  plan_slug             varchar(50) not null   -- card slug or 'custom'
  pricing               jsonb not null         -- the RateCard or CustomTerms used
  period_start          timestamptz not null
  period_end            timestamptz not null
  billed_from           date not null          -- the clipped window, section 8.1
  billed_to             date not null          -- exclusive; check (billed_from < billed_to)
  event_count           bigint not null
  lines                 jsonb not null         -- [{description, events, cents_per_million, amount_cents}]
  usage_cents           bigint not null check (usage_cents >= 0)     -- this period, priced
  carried_cents         bigint not null default 0 check (carried_cents >= 0)
  amount_cents          bigint not null check (amount_cents = usage_cents + carried_cents)
  tax_cents             bigint check (tax_cents >= 0)                -- added on top by Dodo, from the settled payment (section 8.4); null until paid
  covered_by            char(20) references billing_invoices(id)     -- on a deferred row: the invoice carrying it (section 8.7);
                                               -- check (covered_by is null or status in ('deferred', 'paid'))
  currency              varchar(3) not null
  status                text not null          -- section 8.3
  attempts              int not null default 0
  next_attempt_at       timestamptz            -- the first charge, ChargeNoticeDays after the close (section 8.1), then each retry
  last_error_code       text not null default ''
  last_error_message    text not null default ''   -- merchant-facing; never on the wire
  provider_payment_id   text                   -- the latest attempt's payment, cleared at each charge; unique (provider, provider_payment_id)
  provider_invoice_url  text                   -- Dodo's receipt, for ListInvoices
  usage_computed_at     timestamptz not null   -- the meter stamp the count came from
  paid_at, failed_at    timestamptz
  create_time, update_time

  unique (org_id, billed_from)                 -- one close per start day; no day is billed twice because each close starts at the last billed_to (section 8.1)

billing_invoice_events                          -- append-only, like the entitlement history
  id, invoice_id, at, from_status, to_status, actor, detail
```

`amount_cents` is always pre-tax (§3.2); `tax_cents` is what the customer paid
on top, known only once the payment settles. `amount_cents` and `currency` are
honest names while §3 of payments.md (USD only) holds, as `price_cents` is
today. No FK from invoices to
`billing_subscriptions`: a mandate row can be replaced across a provider
cutover and the invoice must keep saying which subscription id was charged.
Invoices are never pruned — they are the ledger billing.md §11.4 promised.

### 8.3 The state machine

```
open ──charge──▶ charging ──payment_id──▶ charged ──webhook/poll──▶ paid
  ▲                 │                          │                      ▲
  │   (ambiguous)   │ settle by listing (8.4)  └──▶ failed ──soft──▶ waits for next_attempt_at
  └─────────────────┘                                └──hard, or 4th──▶ uncollectible ──new card, invoice retry──▶ open
charging ──▶ failed          (the charge was refused outright)
open, failed ──▶ charging    (a retry goes straight back; there is no open hop)
failed ──new card──▶ open    (next_attempt_at = now)
close, nothing to bill ──▶ waived
close, past the catch-up window ──▶ waived             (8.1; detail: dropped)
close, total < DeferUnderCents ──▶ deferred            (8.7; carried by a later close)
deferred ──carrier paid──▶ paid                        (detail: covered by <id>)
deferred ──carrier void──▶ deferred                    (uncovered; the next close carries it again)
deferred ──sweep, total < WaiveUnderCents──▶ waived    (8.7; no live mandate or deal, or the period bound)
open, failed, uncollectible, uncovered deferred ──operator──▶ void
paid ──refund.succeeded, full──▶ refunded          (a partial refund is recorded and leaves it paid)
```

`paid` lands from **every** state but `paid`, `refunded`, `waived` and `void`,
not only from `charged`: a late webhook for a charge that settle already
reopened has to land, or the next pass charges the customer twice. That edge is
load-bearing. A success that cannot land — on a `void` invoice, or a second
payment on one already `paid` — is money taken with no bill behind it: it is
appended to `billing_invoice_events` and reported as `duplicate` (§11) for a
refund by hand, never dropped. On a voided carrier it matters twice, since the
void uncovered the rows that payment already paid for.

Terminal: `paid`, `refunded`, `waived`, `void`, and `uncollectible` until a new
payment method arrives. `deferred` is parked, not terminal: it leaves only
through the invoice that carries it, a sweep, or the operator, and it is never
charged on its own. `void` refuses `charging` and `charged` — a charge in
flight has to settle first — and refuses `paid`, whose only onward state is
`refunded`, written by the refund webhook. It also refuses a `deferred` row an
invoice is carrying, whose cents are fixed inside the carrier's
`amount_cents`: void the carrier instead. Every transition appends to
`billing_invoice_events` with an actor — the pass, a webhook id, or the
operator.

### 8.4 Charging, without an idempotency key

Dodo's charge endpoint takes no idempotency key (verified against SDK
v1.115.0: no such request option exists), so a call that times out after Dodo
created the payment is indistinguishable from one that never arrived.

**The SDK's own transport retry is therefore disabled on this one call**
(`option.WithMaxRetries(0)`). Its default is 2 retries on a nil response — a
connection reset; a timeout returns without retrying — and on 408/409/429/5xx,
so leaving it on would re-POST the charge and take the money up to three times
while pug saw a single error. Reads keep their retries.

The pass therefore:

1. Sets `charging` and **commits** (invariant 3).
2. Calls `POST /subscriptions/{sub}/charge` with `product_price =
   amount_cents` (Dodo takes the smallest currency unit as an integer — whole
   cents, which is why §3.3 rounds before this point), `product_currency = USD`,
   `product_description = "Pug: 2,340,000 events, 17 Aug to 16 Sep 2026"` —
   plus ", and $0.37 carried from 2 earlier periods" when `carried_cents` is
   non-zero (`billed_to` is exclusive, so the text names the last day billed),
   and **explicit metadata** `{org_id, invoice_id, period_start}` — the charge
   inherits the subscription's metadata only when none is passed, and pug needs
   the invoice id on every payment webhook.
3. On a response: `charged` + `provider_payment_id`. A decline is an
   **allow-list of one**: `402 Payment Required`. Every other 4xx is pug's
   problem, not the card's, and is treated as ambiguous — a rotated key, a rate
   limit, or a route or content type a dependency bump changed would otherwise
   dun every customer at once, over 17 days, behind a green pass. On anything
   ambiguous — a non-402 4xx (its status kept in `last_error_code`), timeout,
   5xx, connection reset — **leaves the row in `charging`**. A `404` claims the
   mandate is not chargeable; it is acted on only when `FetchSubscription`
   **corroborates** it by reading the subscription back in a non-live status.
   A second `404` corroborates nothing — a flipped environment answers both
   calls the same way — so it counts as `Unreadable` (§11): acted on, it would
   write off every open invoice in the deployment, one anniversary at a time.
4. **Settles `charging` rows older than a few minutes by reading**:
   `Payments.List(subscription_id, created_at_gte = a minute before the row
   entered charging)` and look for `metadata.invoice_id`. From the charging
   instant, not the invoice's creation, since an earlier attempt's payment
   shares the invoice id and would settle this one; the minute absorbs clock
   skew. The list has no promised order and a retry after a decline shares the
   invoice id with the attempt that failed, so the match is the **newest
   payment that has not failed** — succeeded or still processing — not the
   first one seen. Found → adopt the payment id (`charged`), and polling
   settles it; not found → `open`, and the next tick charges again. More than
   one such payment for one invoice is reported as `duplicate` (§11). **The
   reopen counts an attempt**, so that cycle is bounded by `MaxChargeAttempts`
   (§13) and ends in `uncollectible` rather than re-charging every hour
   forever — each lap POSTs a charge that may really take money. A charge
   answered with a non-402 4xx is the exception: the provider answered, so
   nothing was taken, and the fault is pug's, so its reopen counts no attempt
   and the pass stays red instead of spending the customer's attempts. A retry
   after a *read* is the only retry there is (invariant 7).
5. On marking an invoice `paid`, the payment's `total_amount − tax` and
   `currency` are checked against `amount_cents`: `total_amount` includes tax,
   and pug billed the pre-tax figure. A mismatch does not block the transition —
   the money moved — but it is appended to the invoice's events and reported as
   `amount_mismatch` (§11), because nothing else would notice a charge that
   took the wrong amount. The list carries `total_amount` but no `tax`, so the
   payment it settles on is read whole first, or the check would never run on
   this path. The payment's `tax` is stored as `tax_cents` at the same time.

`payment.succeeded` / `payment.failed` deliveries — stored and ignored today —
now settle invoices by `metadata.invoice_id`, through the existing inbox, but
only when the payment's `subscription_id` is the invoice's `provider_sub_id`.
The id alone names an invoice rather than proving one: static payment links
accept `metadata_*` parameters (payments.md §8), so without the check a cheap
link purchase could mark a large invoice paid. A payment that fails the check
is stored, marked processed and not applied, like any unattributable delivery.
As with `ConfirmCheckout`, the webhook is not the only route: the pass also
polls `Payments.Get` for `charged` rows older than an hour, so a deployment
with no reachable webhook URL still learns whether it was paid.

### 8.5 Retries and dunning are pug's

Dodo states it does not retry failed on-demand charges. Pug owns the policy,
and follows Dodo's own recommendation:

| Outcome | Action |
|---|---|
| soft decline — **any code not in the hard list**, an unknown code included | `failed`; retry at +3d, +7d, +7d after the first attempt (4 attempts over 17 days), then `uncollectible` |
| hard decline — the enumerated list, and only it: the six Dodo's on-demand guide says never to retry, `STOLEN_CARD`, `LOST_CARD`, `PICKUP_CARD`, `DO_NOT_HONOR`, `FRAUDULENT`, `AUTHENTICATION_FAILURE` | `uncollectible` immediately; retrying damages authorization rates |
| the charge exceeds the mandate's ceiling (§8.7; an Indian-card e-mandate registers a maximum) | `uncollectible` at once, under its own reason — the same amount fails again until the customer re-authorizes, so the banner says "re-authorize", not "declined". Until §18.2 learns the error Dodo returns, it rides the ambiguous path and lands there after the fourth attempt |
| a new payment method (`subscription.update_payment_method`) | every `failed`/`uncollectible` invoice → `open`, `next_attempt_at = now`; `attempts` is not reset, so the new card gets one attempt before a failure is final again |
| mandate cancelled / expired | open invoices → `uncollectible`; a finding |

`last_error_message` is merchant-facing and never crosses the wire; the
dashboard gets a reason — `DECLINED`, or `REAUTHORIZE` for the mandate ceiling —
and a link to the portal (§9). The operator reads the code and the message in
`pug billing show --invoices` (§12).

**Nothing is enforced.** A `PAST_DUE` org keeps sending events and keeps
being invoiced; the cost of dunning is a banner and an email, never a degraded
product (payments.md §11, unchanged). Whether pug should ever cancel a mandate
itself after the fourth failure is a decision (§19.8); the recommendation is
no — a cancelled mandate is one that can never be retried.

### 8.6 What the ledger derives

- `BillingStatus` gains `PAST_DUE` (additive enum value): any `failed` or
  `uncollectible` invoice, never a `deferred` one. Derived at read time from
  the ledger, so it clears the instant a retry succeeds, with no sweep.
- `NextChargeAt` = the next scheduled charge: the earliest `next_attempt_at`
  among the org's `open` and `failed` invoices, otherwise the current
  `period_end + grace + ChargeNoticeDays`. The dashboard shows it as "next
  charge".
- The running estimate (§9) is `Price(card, current period usage)`, or
  `PriceCustom(terms, …)` for a deal, picked from the resolved entitlement as
  the invoice's is (invariant 4) — over the live `usage_periods` number, with
  its three-state freshness carried through — plus the uncovered deferred
  balance (§8.7), read off the ledger.

### 8.7 Small invoices and the provider's fee

Dodo's price, from its pricing page as read on 2026-09-15: **4% + 40¢ per
domestic US card transaction**, **+1.5%** on a card from outside the US,
**+0.5%** on "subscriptions, addons and usage based billing", **$1 per
refund**, **$30 per dispute**, and **$5 per payout under $1,000**. Four things
its pages do not say, each assumed here in the safe direction and listed for
§18.2: whether an on-demand charge attracts the 0.5% (assume it does); whether
a declined attempt costs anything (assume not — if it did, every dunning lap on
a small invoice would be a fee, and the thresholds below would need to rise);
whether the 4% is taken on the pre-tax amount or the tax-inclusive total
(immaterial to the thresholds); and whether an Indian card authorizing a
**USD** mandate gets the ceiling Dodo documents for INR e-mandates — the bank
registers `max(mandate floor, amount)`, default ₹15,000, and a charge above it
"will fail and the customer must re-authorize". Dodo refuses a card payment
under 50¢, just above break-even: that blocks the charges that lose money
outright but not the ones that are mostly fee, and a refusal that is not a 402
would ride §8.4's ambiguous path. The guard has to be pug's.

**The fixed 40¢ is the whole problem.** A charge of `A` cents nets pug
`0.955·A − 40` on a domestic card and `0.94·A − 40` on an international one:

| charge | fee (domestic) | pug keeps | take |
|---|---|---|---|
| $0.15 | $0.41 | **−$0.26** | — |
| $0.42 | $0.42 | $0.00 | ≈100% |
| $1.00 | $0.45 | $0.55 | 45% |
| $2.00 | $0.49 | $1.51 | 25% |
| $5.00 | $0.63 | $4.37 | 12.5% |
| $10.00 | $0.85 | $9.15 | 8.5% |
| $20.00 | $1.30 | $18.70 | 6.5% |
| $100.00 | $4.90 | $95.10 | 4.9% |

Break-even is 42¢ (43¢ on an international card). Under it pug pays Dodo to
collect; under a few dollars the fixed part still costs more than the
percentage does. The block design never met this — its smallest non-zero
invoice was $5 — but per-event pricing makes a 15¢ month ordinary: 103,750
events is $0.15, and 100,050 is a fifth of a cent.

**The rule.** Let `balance` be the sum of `usage_cents` over the org's
`deferred` invoices with no `covered_by`, and `total = usage_cents + balance`
for the period being closed. Three placeholder constants (§13), each pinned to
Dodo's fee schedule and to be moved if it moves:

| at close | the new row | the carried rows |
|---|---|---|
| `usage_cents = 0`, not a sweep | `waived`; the balance waits | untouched |
| `total < DeferUnderCents` ($5), not a sweep | `deferred`, carrying nothing | untouched |
| `total ≥ DeferUnderCents` | `open`, `carried_cents = balance` | stamped `covered_by`: `paid` when it is paid, uncovered if it is voided |
| sweep, `total ≥ WaiveUnderCents` ($1) | `open`, carrying, whatever the total | as above |
| sweep, `total < WaiveUnderCents` | `waived` | `waived` |

A **sweep** is the final close of a cancelling mandate (§7.3), a close at
which the balance would span `MaxDeferPeriods` (12) periods — this one and the
org's oldest uncovered `deferred` row both counted — or any close with no live
mandate and no deal in force, older periods a catch-up closes included.
A deferred row is priced, snapshotted and listed like any other; it is simply
never charged on its own — one transaction, one 40¢, for however many periods
it took to reach $5. An org at 162,500 events a month ($2.50) is charged
$5.00 every second month; one at 112,500 ($0.50) once every ten. $1 sits above
break-even on either card type, so a sweep never charges at a loss, and
because a balance is under $5 by construction the write-off per org per sweep
is under $5 and usually under $1. Without the period bound a 5¢-a-month org
would be covered by an invoice eight years later; the bound makes that a 60¢
write-off at month twelve.

**What the ledger says.** `deferred` never contributes to `PAST_DUE` (nothing
was asked of the customer), and the estimate (§9) carries the balance and the
threshold beside this period's amount, so "no charge this month, $3.70
carried" is a sentence the page can form. `NextChargeAt` stays the next
scheduled charge; whether the next close charges at all depends on the
balance, which the page also has.

**Refunds and disputes have the same shape.** A refund costs $1 at Dodo plus
whatever it keeps of the fee (its page does not say), so a credit for a small
mistake — a duplicate delivery, an erasure after invoicing (§17.2) — is worth
more as a **negative carried balance** on the next invoice than as a refund;
the ledger can hold it and the `invoice credit` command is §19.13's, not built
here. A $30 dispute on a $5 invoice is why the charge description names the
events and the period (§8.4), and why a customer-set spend alert (§15, item 6)
is worth building soon.

**The mandate ceiling** is the one edge that is neither a decline nor a fee.
If an Indian card's USD mandate carries one, an invoice whose charge, tax
included, is above it fails the
same way on every attempt until the customer re-authorizes through a new
checkout, so §8.5 maps it to `uncollectible` at once under its own reason and
the banner says "re-authorize", not "card declined". VERIFY the error Dodo
returns, whether a USD mandate carries the ceiling at all, and which webhook a
re-authorization produces (§18.2).

## 9. RPC surface

Additive on the wire, apart from the tier fields §21 reserves (the app is live; nothing is renumbered):

| RPC | Spec | Change |
|---|---|---|
| `GetBillingStatus` | viewer floor, unchanged | `+ rate_card` (free_events, tiers of `{up_to_events, cents_per_million}`), `+ custom_terms` (flat_fee_cents, rate_cents_per_million, included_events), `+ chargeable`, `+ next_charge_at`; `status` may be `PAST_DUE`, with `+ past_due_reason` (`DECLINED` or `REAUTHORIZE`, §8.5); `plan.price_cents` and `current_period_end` reserved |
| `GetUpcomingInvoice` | **new**, `OrgGated(ResourceBilling, ActionRead)` | the current period priced so far, broken down by band: `event_count`, `lines` of `{description, events, cents_per_million, amount_cents}` (the free band included, at 0), `usage_cents`, `carried_cents` (§8.7's uncovered balance), `amount_cents`, `defer_under_cents`, `usage_computed_at`, `counted` — the same three-state freshness as `GetUsage`, so "unknown" never renders as $0; every amount pre-tax, since the tax is known only when Dodo charges |
| `ListInvoices` | **new**, admin-only via `ResourceInvoice` + `ActionRead` granted to admin | closed periods newest-first: `usage_cents`, `carried_cents`, `amount_cents`, `tax_cents`, status (`DEFERRED` included, with `covered_by`), `provider_invoice_url` (Dodo's receipt), dates, and `next_attempt_at` — when an open invoice will be charged or a failed one retried. Never `last_error_message` |
| `CreateCheckoutSession` | unchanged shape | opens a mandate-only checkout; `plan_slug` must be the current card or `custom` |
| `RemovePaymentMethod` | **new**, admin (`ActionCreate` alongside checkout) | §7.3 layer 3 |
| `ListPlans` | unchanged spec | `PlanOption + rate_card`, or `+ custom_terms` for a deal; `price_cents`, `included_events` and `retention_days` reserved |
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

The rate card makes it a number: a new card version with `FreeEvents: 0`
charges new orgs from their first event, and every pinned org keeps its free
100k until an operator moves it. That is the whole pricing change.

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

Until then the invoicing pass's last stage reports **unbilled usage**: a closed period over
the current card's free allowance for an org with no mandate. That is the
number that says how much the free tier is costing.

## 11. The invoicing pass

`cmd/cron/billing-invoice` (image `cron-billing-invoice`), shaped exactly like
the two passes that exist: one-shot, `cron.WithLock` on its own
`JobBillingInvoice` key, `passTimeout`, non-zero exit on failure, lock
contention exits 0, a root span before config. With billing off it finds
nothing to do and exits 0, since its CronJob is scheduled before the flag flips
(§18). **Hourly**: anniversaries are
spread across the month and retries are dated to the hour. Under the lock, in
order:

1. **Close** every period that is due and safe (§8.1), deferring, carrying or
   sweeping per §8.7.
2. **Charge** every `open` or `failed` invoice with `next_attempt_at ≤ now`
   and a live mandate (§8.4).
3. **Settle** `charging` rows by listing, `charged` rows by polling (§8.4).
4. **Pin** `next_billing_date` on mandates whose next charge moved (§7.3).
5. **Report**: periods held back by a stale meter (`held`), write-offs
   (`uncollectible`) and the subset whose mandate is gone (`mandate_gone`), a
   due period that reached the end of the catch-up window unbilled (`dropped`),
   closes deferred and balances swept or waived (§8.7), deals waiting on a card
   (§19.15), payments that took the wrong amount (`amount_mismatch`) or landed
   with no bill behind them (`duplicate`, §8.3), and unbilled usage (§10) —
   counted and logged, exported as `billing.invoice_pass_total{outcome}` and
   `billing.invoices_total{status}`. A webhook that meets an `amount_mismatch`
   or a `duplicate` records it on the invoice's events and logs it as an error.
   The counters are emitted even when the pass returns an error, since the work
   already done is what says where it stopped.
   The pass logs the **effective grace** it ran with: the meter and the invoicer
   are separate CronJobs reading separate env blocks, and nothing makes
   `PUG_USAGE_RESCAN_DAYS` agree across the two; §17.1 says what a mismatch
   costs.

A separate binary rather than a stage of `billing-reconcile`: reconcile is
"nothing auto-fixed" and read-mostly, this one moves money, and the two want
different cadences and different alerts. The cost is another CronJob in
gitops. Decision §19.9.

**Every failure that is a person's to fix is a finding, not an exit code**
(a declined card, a cancelled mandate, a period the meter has not reached).
The pass exits non-zero when it could not read or write — Postgres, or **any**
provider call (`Unreadable`, matching the reconcile pass: one org charging
successfully says nothing about the ones that did not) — or when it left a
charge unresolved (`Ambiguous`), since that is money in an unknown state, or
dropped a period (`dropped`), which no later pass will bill. It also exits
non-zero when more than ten mandates were written off as gone in one
pass: at that scale it is pug's own fault, not ten customers'. So a red CronJob
means the pass itself is broken, not that a customer's card is. A stored invoice
this build cannot read (`Undecodable`) is the same kind of thing: the row is
skipped wherever it is met — so it is never charged and never settled — and the
ledger read refuses rather than returning a customer's history one row short.

## 12. Operator CLI

```shell
pug billing show <org-id> [--history] [--invoices]
pug billing set  <org-id> --plan usage-2026-09-1|custom --actor <who>
                          [--flat-fee 40000] [--rate-per-million 3000] [--events 5000000]
                          [--retention-days N] [--name ...] [--anchor-day N]
                          [--until 2027-01-01] [--note ...]
pug billing extend-trial <org-id> --days 30 --actor <who>
pug billing clear <org-id> --actor <who>
pug billing preview <org-id> --events 2340000          # Price or PriceCustom on the org's card or terms
pug billing invoice void  <invoice-id> --actor <who> --note "..."
pug billing invoice retry <invoice-id> --actor <who>   # uncollectible → open, now
```

- `--flat-fee` is USD cents and `--rate-per-million` USD cents per million
  events (`3000` is $30 per million, $0.00003 an event); on `--plan custom`,
  omitted keeps and `0` clears, the same merge rule as every other flag. On any
  other plan — a card, or `--plan ''` — they are force-cleared whether or not
  the flag was passed, so no pin can leave a price behind for the next custom
  set to satisfy its guard with. `--provider-product` is gone. Starting a deal,
  or renewing a lapsed one, stamps `terms_effective_at`, which is the day the
  deal is invoiced from (§5); any other write does not.
- `set --plan custom` refuses a row with neither fee nor rate. `--events` is
  any whole number of events.
- `--plan ''` removes a pin and keeps the rest of the row. `free` and `trial`
  are no longer plans (§21): a trial is `extend-trial`, and a comp is a card pin
  with a bigger `--events`.
- `show` prints `RESOLVED` with the card or terms and the next charge date,
  `STORED` with the money columns, and `--invoices` the ledger newest-first
  with status, attempts and the last error's code and message — the operator's
  view of a dunning conversation.
- `preview` is how a deal is sanity-checked before it is set: it is
  `PriceCustom` on the org's own terms or `Price` on its **mandate's** card,
  whichever the org resolves to, which is what the customer will be charged.
- `void` is the only way to stop a wrong charge before it happens, and an
  invoice's notice window (§8.1) is when it can; it never
  calls the provider and it cannot touch a `paid` invoice, whose only onward
  state is `refunded` and whose only writer is the refund webhook, or a
  `deferred` row an invoice is carrying (§8.3). `retry` is for the customer who
  fixed their card by phone.

Every write still appends to `billing_entitlement_history`, which gains the
two money columns; invoice writes append to `billing_invoice_events`.

## 13. Configuration

| Var | Default | Meaning |
|---|---|---|
| `PUG_BILLING_ENABLED` | `false` | unchanged |
| `PUG_BILLING_PROVIDER` | `""` | unchanged |
| `PUG_DODO_API_KEY`, `PUG_DODO_ENVIRONMENT`, `PUG_DODO_WEBHOOK_SECRET` | | unchanged |
| `PUG_DODO_MANDATE_PRODUCT` | — | the one on-demand product every org authorizes against. **Replaces** `PUG_DODO_PRODUCT_STARTER/GROWTH/SCALE`. Absent ⇒ not purchasable, as a missing tier key is today |
| `PUG_USAGE_RESCAN_DAYS` | `2` | now also the invoicing grace (§8.1). The invoice pass resolves it through the meter's own function (unset, 0 or negative → 2), never a raw read, but nothing makes the two CronJobs see the same value, so the pass logs the grace it ran with (§11) |
| `DeferUnderCents` | 500 | a Go const, placeholder: under it a close is `deferred` and carried forward (§8.7) |
| `WaiveUnderCents` | 100 | a Go const, placeholder: under it a sweep writes the balance off instead of charging at a loss (§8.7); must stay at or above Dodo's 50¢ card minimum, which also clears the ~42¢ break-even |
| `MaxDeferPeriods` | 12 | a Go const, placeholder: how many periods a balance may span before a close becomes a sweep (§8.7) |
| `MaxChargeAttempts` | 4 | a Go const: charge attempts before a soft decline is final (§8.5); a new card or `invoice retry` buys one more |
| `ChargeNoticeDays` | 3 | a Go const, placeholder: days between a natural close and its first charge, the notice a billing email will announce (§8.1); a close forced by a going mandate charges at once |

## 14. Migrations 021 and 022

- `billing_entitlements`: `+ flat_fee_cents bigint check (>= 0)`, `+
  rate_cents_per_million bigint check (>= 0)`, `+ terms_effective_at
  timestamptz`; `custom_needs_quota` replaced by
  `custom_needs_price`, which checks `> 0` rather than merely not-null so a
  hand-written zero cannot turn a deal into a free tier nobody agreed to (§5).
  No constraint on the allowance's shape: any whole number of events.
  `plan_slug` loses its not-null (§21); the history's is already nullable. The
  history table mirrors the money columns and gains a `deleted` marker, which
  its deletion constraint is rewritten on.
- `billing_subscriptions`: `+ on_demand boolean not null`, `+
  cancel_at_period_end boolean not null` (drives the early close, §7.3), `+
  ended_at timestamptz` (clips the last invoice), `− price_cents` (§21).
- `billing_checkout_sessions`: `+ plan_slug varchar(50) not null` (§4), with
  no default: there is no row to backfill, and with billing off nothing inserts
  one.
- `billing_invoices` (`usage_cents`, `carried_cents`, `covered_by`, `tax_cents`,
  no block column, unique on `(org_id, billed_from)`, no FK to `orgs`),
  `billing_invoice_events` (§8.2).
- `cron.LockBillingInvoice` joins the advisory-lock iota.

**Only `provider_product_id` waits for 022.** Production has no entitlement,
subscription or checkout row, so 021 reshapes those tables freely, drops
included. That column is the exception: `main`'s `GetBillingStatus` runs
`GetOrgEntitlement` on every dashboard load, billing on or off, and the query
names it. Dropping it in 021 would fail those reads on the pods still serving
while the migration runs, however empty the table. **Migration 022**, one
release later, drops it from both entitlement tables. `price_cents` needs no
such wait: outside the operator's own `pug billing` commands, `main` reads
`billing_subscriptions` only with billing on or a provider configured, and
production has neither.

No data migration and no backfill: production has billing off and every
billing table empty. The three tier product keys go from any deployment that
set them (gitops sets none today); the Dodo products behind them are retired
in Dodo's dashboard by hand.

## 15. Alternatives considered

On-demand charging is the vehicle. Here is why the alternatives lose, and the
choices made around it.

| Strategy | Why not |
|---|---|
| **Dodo usage meters** (`/events/ingest`, price per unit + free threshold) | Per-unit only — no tiers, graduated or otherwise. And "events timestamped more than 1 hour in the past are rejected": pug's meter is a revising batch that re-reads two days back, which can never be replayed into a one-hour window. Dodo would also need every event, not a daily count. |
| **Dodo credit-based billing with overage** (v1.86) | Credits per cycle + overage at one rate. Models "100k free then $X" but not six tiers, and pug still pushes usage to Dodo. |
| **Recurring subscription + add-on quantity** (the discarded overage design) | Gated on the unverified `do_not_bill` semantics, couples the charge to Dodo's `next_billing_date`, and graduated tiers need one add-on per tier. On-demand strictly dominates for pure usage pricing. |
| **Prepaid wallet / credits** (`customer_balance_config`) | Cash up front, no dunning — but it is "buy credit up front", not "pay for what you used", and Dodo's wallet semantics are undocumented. Worth revisiting as the *entry ticket* if the free tier goes (§10): "prepay $10 to start" is a friendlier gate than "add a card". |

And around the on-demand design:

1. **Graduated over volume** (§3.2) — volume would let a customer pay less by
   sending more.
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

Tests sit where `main`'s billing tests already do — `internal/core/billing`,
`internal/deps/dodo`, `internal/app/cron/billing` and the invoice pass beside
it, the dashboard handlers, `internal/app/billing` and `cmd/pug` for the CLI,
and the authz tests — each container package keeping `TestMain` and no
`t.Parallel()`. In `internal/core/billing` the provider stays a fake.

- **`Price`** — table-driven, written first: every worked example of §3.2, the
  100,000th event free and the 100,001st charged, each tier bound at 2M /
  15M / 50M / 100M / 250M, half-up rounding per line (100,124 events → $0.00,
  100,125 → $0.01), lines summing to the total, `FreeEvents: 0`, an unbounded
  last tier, no overflow at 10¹² events and the largest rate `SetPlan` accepts,
  and the golden pin on every card's priced fields and `Retired`.
  `PriceCustom`: fee only, rate only, both, allowance larger than usage.
- **Resolution and grandfathering** — a retired card keeps resolving for its
  holder; a new mandate pins the card current when its checkout opened, even
  one retired before the delivery; the entitlement slug wins over
  the subscription's; a live deal under a mandate is priced on its terms and a
  lapsed one falls to the current card; exactly one of `Card` and `Terms` is
  set; the estimate, `preview` and the invoice price one count identically for
  an org on a retired card.
- **Period close** — clipped windows for a mid-period mandate, a mid-period
  cancellation, a trial (one a deal ended included), a deal billed from
  `terms_effective_at` before its card (§19.15), and a deal ending mid-period
  or starting over a card; a stale meter holds the close, and a meter back from
  an outage across an anniversary re-reads the period before it closes; an
  anchor change, an early close, overlapping mandates and a card removed and
  added back bill every day exactly once, priced on the latest mandate; a
  recurring mandate, a status pug has no word for and an undated end bill
  nothing and block nothing; an unpriceable period holds the org; every period
  past the catch-up window is written off once as `waived` with a `dropped`
  event; a natural close not
  charged before `ChargeNoticeDays` have passed, while a close forced by a
  going mandate, `RemovePaymentMethod` and a deal's first card charge at once;
  a cancellation landing inside the window pulling the charge forward; `void`
  inside the window stopping the charge with no provider call; every row of §8.7's
  table: nothing to bill waived, under $5 deferred, a balance that tips the
  total over $5 carried and covered, a covered row paid with its carrier and
  uncovered by a void, eleven periods deferred and the twelfth swept, a gone
  mandate sweeping, a sweep under $1 waived and one over it charged, and the
  final close of a cancellation carrying the balance, a lapsed deal with no
  card sweeping, and a carried row never carried again; a `deferred` row never
  makes `PAST_DUE`; a close starts where an existing row ends.
- **The charge state machine** (Postgres) — every edge of §8.3, including
  `paid` landing on a reopened or `uncollectible` invoice, a success on a
  `void` one reported as `duplicate`, and `void` refusing a covered row;
  `charging` committed before the charge, and a second claim on one invoice
  calling no provider; the ambiguous outcome: a fake that creates the payment
  and then errors, settled by listing and never charged twice; a `processing`
  payment adopted, and two payments that have not failed reported; a fake that
  errors without creating one, re-opened and charged once; a non-402 4xx
  reopened without counting an attempt; a `404` acted on only when the
  subscription reads back non-live, and a second `404` held; the attempt cap;
  the amount check on `total_amount − tax`, with `tax` stored as `tax_cents`;
  each of the six hard-decline codes as a string literal, an unknown code soft,
  and the retry dates; a new payment method re-opening `uncollectible`.
- **Webhooks** — `payment.succeeded`/`.failed` settle by `metadata.invoice_id`,
  ignore a payment carrying none, and refuse one whose `subscription_id` is not
  the invoice's mandate; a partial refund leaves the invoice `paid`; a
  non-on-demand or tax-inclusive subscription is rejected by the webhook and
  by `ConfirmCheckout`; `past_due` maps live.
- **The Dodo adapter** (`internal/deps/dodo`, against an httptest server) — the
  charge POSTs exactly once against a 502 and a 429, sends whole cents and
  `metadata.invoice_id`, and maps only a 402 to a decline. The provider fake
  cannot see the SDK's own retries.
- **Cancellation** (§7.3) — `next_billing_date` pinned on activation and after
  every invoice; the early close only when the mandate ends first;
  `RemovePaymentMethod` refusing on any `open`, `charging`, `charged` or
  `failed` invoice, on an unclosed previous period, and again on a repeat call
  after `charged`, and counting a final invoice the sweep waived as settled.
- **The invoicing pass** (§11) — `Unreadable`, `Ambiguous`, `dropped` and more
  than ten gone mandates exit non-zero; ten gone mandates, lock contention and
  billing off exit 0; a failing pass still emits its counters; an
  `Undecodable` row is counted and never charged.
- **Operator CLI** (§12) — the money flags keep when omitted and clear on `0`;
  a card pin or `--plan ''` force-clears them; `void` refuses `charging`,
  `charged`, `paid` and a covered row; `retry` works only from `uncollectible`.
- **Migrations** (§14) — a raw zero-price custom insert is refused by
  `custom_needs_price`; `provider_product_id` still exists after 021.
- **RPC freshness** — `GetUpcomingInvoice` carries the three usage states
  through and never renders an unknown count as $0.
- **Authz** — the registry and policy tests fail until `ResourceInvoice` and
  the three new procedures are entered, and `ListInvoices` and
  `RemovePaymentMethod` join `TestRoleGatedAdminOnlyRPCs`' admin-only list, the
  only test that catches one left on the viewer floor.

## 17. Known imprecision

1. **Events arriving more than `grace` days after their `occur_time` are never
   billed.** They land in `usage_daily` (the dashboard sees them) but the
   invoice has closed. Errs in the customer's favour, at most their rate each.
   If the meter and the pass run with different `PUG_USAGE_RESCAN_DAYS` (§11),
   only events within the smaller value are sure to be billed: a shorter grace
   closes while the meter still re-reads the period, and a shorter meter window
   stops re-reading it before the close.
2. **An erasure after invoicing credits nothing.** The invoice froze its count
   (§8.1). A credit note is a refund in Dodo's dashboard ($1 a time, §8.7) and
   `invoice void` — or a negative carry once §19.13 lands.
3. **The estimate and the invoice can differ** — the estimate prices the live
   `usage_periods` number, the invoice sums the clipped window at close. Same
   function, different inputs, documented on the page ("estimate").
4. **Each line rounds to whole cents on its own** (§3.3): at most half a cent
   per line either way, and an org sitting a few events past the free 100k
   rounds to $0.00 every month and is never billed.
5. **Tax is added on top, by Dodo.** Pug prices, estimates and invoices
   pre-tax (§3.2). The tax lands on the charge, the receipt and `tax_cents`;
   the estimate cannot include it, because it depends on the billing address
   and tax ID Dodo holds.
6. **The invoice is not dated on the anniversary, and the charge is later
   still.** The period is anniversary-aligned; the invoice follows it by the
   grace, the charge follows the invoice by `ChargeNoticeDays` (§8.1), and
   `next_charge_at` says when the money moves.
7. **`RemovePaymentMethod` can refuse.** A decline, an ambiguous charge, a
   meter that has not reached the period or a previous period still inside its
   grace leaves the mandate live and returns
   `BILLING_FINAL_PERIOD_UNSETTLED`. The admin retries once the charge settles,
   or voids the invoice. Cancelling first would write the period off.
8. **Cancellation write-off** (§7.3): at least a day of usage, a month if the
   `next_billing_date` pin turns out unsupported — plus a carried balance under
   $1, waived at the final sweep (§8.7).
9. **A deferred balance is collected late**, up to a year after it was earned,
   and a customer who leaves mid-way pays it in one lump on the final invoice. The dashboard shows the balance the whole time.
10. **A term change prices every day not yet invoiced**: the current period from
    its start, and the previous one while it is inside its grace. Repricing or
    clearing a deal, repinning a card and renewing a lapsed deal all reach back
    that far; a cleared deal with no card bills those days not at all.
    `extend-trial` on a paying org forgives them, and `clear` on an org with an
    extended trial bills its trial days on a live card.
11. **A fee and an allowance are per invoice, not prorated.** A deal starting
    or an anchor moving mid-period invoices a stub with a whole flat fee or card
    allowance, the card days either side of a deal inside one period each get
    their own allowance, and a card-pin comp is priced as it stands at the
    period's end.
12. **Deleting a project un-bills it.** Its `usage_daily` rows go with it, so a
    period not yet closed bills none of its events.
13. **A deferred balance with no later close stays deferred.** Only a close
    sweeps, so when the org's last one ran with a mandate or deal still in force
    — a cancellation read after it, or a deal cleared with no card — nothing
    carries the balance.
14. The meter's own imprecisions (`usage.md` §8) are inherited unchanged.

## 18. Rollout

1. Migration 021, then the server, `cron-usage` (its catch-up, §8.1),
   `cron-billing-reconcile` and the new `cron-billing-invoice` images. The
   ordering constraint is the usual one for a new column read by a live path:
   021 before the images. Migration 022 follows in the next release, once no
   pod runs `main`'s billing queries.
2. In Dodo **test**: create the mandate product; verify the marked VERIFY items
   (§7.1 session shape, §7.3 `next_billing_date`, the checkout page's rendering
   of a $0 authorization, tax added on top of a charge and what a tax ID
   changes, and §8.7's unknowns — the 0.5% on an on-demand
   charge, a fee on a decline, the fee's tax base, the mandate ceiling on a USD
   mandate), and what `cancelled_at` holds once a scheduled cancellation takes
   effect: it is set when the cancellation is requested, and a close stops
   billing at it, so a request date kept there leaves the days to the actual end
   unbilled; run a mandate → close → charge → `payment.succeeded` → portal
   cancel cycle end to end, and a $1.00 charge to read the fee Dodo actually
   takes off a small one.
3. gitops: `PUG_DODO_MANDATE_PRODUCT`, and CronJobs for reconcile and the
   invoice pass, the invoice one hourly. It sets no tier keys today.
4. Flip `PUG_BILLING_ENABLED` when the dashboard side (`../app`: rate card,
   estimate, invoices, "Add payment method" replacing "Upgrade") has landed.
5. `CLAUDE.md` pointers and the fold-in of this document into the three docs
   land with the code, as §22's fifteenth commit.

## 19. Decisions

Each has a recommendation; the plan above assumes it.

1. **Graduated tiers** (not volume). Recommend yes.
2. **Exact per-event pricing, each line rounded half-up to whole cents**
   (§3.3), not blocks. Recommend yes; it removes every cliff.
3. **Pin the rate card at the mandate checkout** (not org creation). Recommend
   yes.
4. **Clip the first and last invoice to the mandate's days**, at day grain.
   Recommend yes. Alternative: bill the whole period the card was added in.
5. **Keep the 14-day trial as a no-charge window.** Recommend yes; it costs
   nothing while a free allowance exists and is the evaluation period if it goes.
6. **`GetUpcomingInvoice` on the viewer floor, `ListInvoices` admin-only.**
   Recommend yes.
7. **Defer under $5 and carry forward; at a sweep charge from $1 and waive
   below it; sweep once a balance spans twelve periods** (§8.7). Recommend
   yes. Alternatives: waive everything under a threshold (simplest; gives away
   up to $5 a month per org, a hidden 125k events on top of the free 100k), or a minimum
   invoice of $5 (collects, but bills a 15¢ month as $5, the opposite of what
   the rate card promises).
8. **Dunning: 4 attempts over 17 days, then `uncollectible` + banner + email;
   pug never cancels the mandate itself.** Recommend yes.
9. **A separate `cron-billing-invoice` binary** rather than a stage of
   reconcile. Recommend yes, at the price of another CronJob.
10. **A lapsed custom contract falls to the current card**, not to free.
    Recommend yes.
11. **Placeholder prices and bounds** — 100k events free a month, then $40 per
    million to 2M, each band about 30% cheaper than the last: $28 / $20 / $14 /
    $10 / $7 per million past 2M / 15M / 50M / 100M / 250M. 5-year retention.
    Placeholders until set; the golden test pins whatever ships.
12. **`RemovePaymentMethod` in this slice** (§7.3 layer 3) or later. Recommend
    this slice: it is the one path where pug controls the order of charge and
    cancel.
13. **Credits as a negative carried balance** rather than a $1 refund (§8.7),
    with an `invoice credit` command. Recommend yes, in a later slice.
14. **The three fee constants** — $5 / $1 / 12 periods. Placeholders; §8.7's
    break-even table is what to move them against, and they move again if
    Dodo's fee does.
15. **A deal recorded before its customer adds a card** keeps its invoices
    `open` until the first mandate arrives, then charges them oldest first,
    and the invoicing pass reports the deal as waiting on a card. Recommend
    yes. The block-model build instead writes each one off as `mandate_gone`
    the moment it closes, which loses the money and counts toward the pass's
    ten-write-offs alarm. Alternative: clip a deal's billable window to its
    mandate like a card org's, which forgives the months before the card.
16. **`free` and `trial` stop being plans**, and `plan_slug` becomes nullable
    so a row can carry a trial end, an anchor day or an override without a pin
    (§21). Recommend yes: the floor plans are where the tier model's bugs
    lived, and nothing in the usage model needs a quota of their own.
    Alternative: keep them as marker slugs with no quota, as the block-model
    build did.
17. **Prices exclude tax** (§3.2): Dodo adds tax on top from the billing
    address, and a business can give its tax ID at checkout. Decided. A
    tax-inclusive price would make the same rate worth less wherever tax is
    higher.
18. **A notice window before the first charge** — `ChargeNoticeDays` (3,
    placeholder) between a natural close and its charge, so the customer can
    see the invoice, and the operator void it, before money moves; the email
    that announces it lands in a later PR. Recommend yes. Alternative: charge
    in the pass that closes the invoice, which leaves `invoice void` no time
    to act.

## 20. What this revision changes

Against the block model built on `feat/usage-billing` (revised 2026-09-15):

| Was | Now |
|---|---|
| `BlockEvents` 100k, usage rounded to the nearest block, `FreeBlocks` | exact events, `FreeEvents` (§3.1) |
| `Tier{UpToBlock, CentsPerBlock}` | `Tier{UpToEvents, CentsPerMillion}` (§3.3–3.4) |
| $50 / $40 / $30 per million (as $5 / $4 / $3 a block), breaking at 1M / 5M | $40 / $28 / $20 / $14 / $10 / $7 per million, breaking at 2M / 15M / 50M / 100M / 250M (§3.2). The free 100k is unchanged |
| `Line{Blocks, CentsPerBlock}`, `Quote.Blocks`, `billing_invoices.blocks` | `Line{Events, CentsPerMillion}`, `Quote.Events`, no block column (§8.2) |
| `block_rate_cents`, the `allowance_blocks` constraint, whole-block `--events` | `rate_cents_per_million`, any allowance, `--rate-per-million` (§5, §12, §14) |
| `MinChargeCents` 50¢ → `waived` | `DeferUnderCents` / `WaiveUnderCents` / `MaxDeferPeriods`, a `deferred` state, `usage_cents` + `carried_cents` + `covered_by` (§8.7) |
| four hard-decline codes | Dodo's six, plus the mandate ceiling (§8.5) |
| `free` and `trial` kept as stored marker slugs | a nullable `plan_slug` (§21, §19.16) |
| the subscription's `price_cents` mirror, `Plan.price_cents` and `current_period_end` on the wire | removed and reserved (§21) |
| `rate_card{block_events, free_blocks, tiers{up_to_block, cents_per_block}}`, `custom_terms.block_rate_cents`, `lines{blocks, cents_per_block}` on the wire | per-event names throughout, `+ usage_cents`, `+ carried_cents`, `+ defer_under_cents`, `+ past_due_reason`, `INVOICE_STATUS_DEFERRED` (§9). None of the block fields reached `main`, so they are replaced, not reserved |
| `unique (org_id, period_start)`; a payment settled on `metadata.invoice_id` alone; a `404` from `FetchSubscription` taken as corroboration | `unique (org_id, billed_from)`, each close starting at the last `billed_to` (§8.1); the payment's `subscription_id` checked; only a subscription read back non-live corroborates (§8.4) |
| the payment's tax used only to check the amount | prices stated excluding tax (§3.2), a tax-inclusive mandate refused (§7.2), `tax_cents` on the invoice (§8.2) |
| an `open` invoice's first attempt dated at its close, so the pass that closes it charges it | `ChargeNoticeDays` after the close, and at once only for a going mandate or a deal's first card (§8.1) |

## 21. What goes

The tier system on `main` leaves pieces the usage model gives no job. They go
in the commits that replace them (§22), and none is kept for compatibility:
billing is off in production and no entitlement, subscription or checkout row
exists there.

**Plans and floors.**

- `starter`, `growth`, `scale` and the `Plan` catalog in `plans.go`, replaced by
  the rate card (§3.4).
- `free` and `trial` as plans. Their 10k and 500k quotas mean nothing once an
  unpinned org gets the card's allowance and trial days are simply not billed
  (§6). FREE and TRIALING stay as statuses. What the slugs did as stored
  markers moves to a nullable `plan_slug`, where null means no pin, so
  `extend-trial`, an anchor day or a retention override needs no stand-in plan,
  and a comp is a card pin with a bigger allowance (§19.16). With them go
  `ErrTrialNotSettable`, the floor-plan rules in `pug billing set`, and the
  history's "a null plan means deleted" encoding, which becomes an explicit
  `deleted` marker.

**The product-per-tier checkout.**

- `PUG_DODO_PRODUCT_STARTER`, `_GROWTH` and `_SCALE`, `ProductIDs` in
  `internal/deps/dodo/products.go`, `Payments.ProductBySlug` and
  `checkoutProduct`. The one mandate product replaces them (§7.1).
- `planForProduct`, and with it the rule that a product only has to resolve to
  grant. The slug is pinned on the checkout session and carried on the
  subscription row, so a delivery's product decides nothing, and reconcile's
  `UnmappedProduct` count goes too.
- The staged product: `provider_product_id` on both entitlement tables,
  `--provider-product`, `GetBillingEntitlementProviderProductID`, and the
  payment-link attribution that trusted `metadata.org_id` paired with it (§5).
- The strand guard: `ErrClearWouldStrandSubscription` in `clear` and `set`, and
  the entitlement lock and re-read in `applySubscription`. Both exist because a
  custom subscription took its quota from the org's row. A cleared org with a
  live mandate now resolves to its pinned card, or to the current card for a
  mandate taken on a deal, and stays chargeable.

**The subscription's price.** `billing_subscriptions.price_cents`, `PriceCents`
on `Subscription` and `SubscriptionEvent`, and the negative-price refusal at the
webhook. A mandate's recurring price is never charged, so mirroring it only
invites someone to read it as the bill.

**Wire fields**, reserved rather than renumbered:

- `Plan.price_cents` (3): no card has a single price.
- `PlanOption.price_cents` (3), `included_events` (5) and `retention_days` (7):
  `rate_card` or `custom_terms` carries the allowance, and retention is five
  years on every card.
- `GetBillingStatusResponse.current_period_end` (10): the provider no longer
  bills on a schedule of its own, and `next_charge_at` is the date that
  matters. The column stays, because the early close reads it (§7.3).

The dashboard hides every billing surface while `billing_enabled` is false, so
a deployed `../app` reading these gets defaults and renders nothing. It drops
the reads when it next regenerates its types.

**Docs**, at the fold-in: `payments.md` §4 (who owns the price), §5.1–§5.2 (the
hand-made product and the payment link) and §16–§17 (the archived build and what
the first build changed); `billing.md` §4's tier tables and §14's divergences;
and the `CLAUDE.md` lines about tiers, staged products and stranding.

**Stays**, because the usage model still leans on it: the webhook inbox and its
CAS, `billing_checkout_sessions` and `checkout_ref` attribution, customer-id
attribution for later deliveries, `ConfirmCheckout`, the portal, reconcile's
other findings, `billing_subscriptions.currency` for the USD lock, the
entitlement history, and trials, anchor days, contracts, display names and
retention overrides.

## 22. Work order

The code lands on a fresh branch off `main`, as the commits below and in this
order. The block-model build on `feat/usage-billing` is the parts bin: most of
it is re-cut and re-priced, not rewritten. Every commit builds and passes `go
test` and `make lint` on its own. golangci's `unused` is on, so an unexported
helper lands with its first caller. Migration 021 is applied nowhere until its
pull request merges, so commits 4 and 5 extend it beside the code its
constraints break.

1. **Design doc.** This file, from `docs/usage-billing`.
2. **Rate card and pricing** (§3). `RateCard`, `Tier` and `CustomTerms`, the
   placeholder catalog, `Price` and `PriceCustom` with per-line rounding,
   catalog validation and the golden pin. Exported and pure, so it lands with
   no caller yet.
3. **Migration 021, the additive half** (§14). The new tables and the columns
   nothing reads yet, `price_cents` dropped along with the subscription price
   mirror that read it, and regenerated sqlc models. Only
   `provider_product_id` waits for 022.
4. **Resolve cards and deals** (§4–§6). 021 gains the `custom_needs_price`
   swap, the nullable `plan_slug` and the checkout session's `plan_slug`, with
   the code and tests each one breaks. The tier catalog and the `free` and
   `trial` plans go, with the floor-plan rules in `pug billing set`. `Resolve`
   returns a card or a deal and `Chargeable`, a live deal is priced on its
   terms and a lapsed one falls to the current card, and grandfathering picks
   the entitlement's slug, then the mandate's, then the current card.
   `SetPlan`'s price guards and `terms_effective_at`; `pug billing set
   --flat-fee --rate-per-million`, `show` and `preview`; `rate_card`,
   `custom_terms` and `chargeable` on
   `GetBillingStatus` and `ListPlans`, with the tier fields reserved. Checkout
   maps every plan to `PUG_DODO_MANDATE_PRODUCT` and pins the slug, so the
   product-per-tier wiring, `planForProduct`, the staged product and the strand
   guard all go in the same commit (§21).
5. **On-demand mandates** (§7.1–§7.2). 021 gains the subscription's
   `on_demand`, `cancel_at_period_end` and `ended_at`. The `mandate_only`
   checkout with tax IDs allowed, a recurring or tax-inclusive subscription
   rejected at the boundary, `past_due` as live, the cancellation fields
   recorded, reconcile findings, and
   `FetchCheckoutOutcome`'s fallback to the customer's subscriptions by
   `checkout_ref`.
6. **Close periods** (§8.1–§8.3). `SumUsageDaily`, the due-and-safe rule with
   the meter catching up after an outage, the clipped billable window starting
   at the last `billed_to`, the pricing snapshot, `waived` at zero, the catch-up
   window, the first charge dated `ChargeNoticeDays` out, and a deal with no
   card invoiced per §19.15.
7. **Defer small invoices** (§8.7). The `deferred` state, `usage_cents` /
   `carried_cents` / `covered_by`, carrying and sweeping, and the three
   constants.
8. **Charge** (§8.4, steps 1–3). The charge call with SDK retries off, the row
   committed before it, the 402-only decline, ambiguity left in `charging`, a
   404 acted on only when `FetchSubscription` reads the mandate back non-live,
   and an org that never had a mandate held rather than written off.
9. **Settle** (§8.4, steps 4–5). Listing to settle `charging` on the newest
   payment that has not failed, a non-402 4xx reopening without an attempt,
   polling `charged`, the `payment.succeeded`, `payment.failed` and
   `refund.succeeded` webhooks with the `subscription_id` check and full
   refunds only, `duplicate`, the amount check on `total_amount − tax` with
   `tax_cents` stored, and deferred rows following their carrier.
10. **Dunning** (§8.5–§8.6). Soft and hard declines, the retry schedule,
    `uncollectible`, reopening on a new payment method, a mandate gone, and
    `PAST_DUE` with its `past_due_reason` read off the ledger.
11. **Cancellation** (§7.3). Pinning `next_billing_date`, the early close, the
    final close as a sweep, and `RemovePaymentMethod`'s charge-then-cancel
    order, refusing while any invoice is unsettled or the previous period is
    unclosed, and an invoice in its notice window charged at once when the
    mandate is going.
12. **The invoicing pass** (§11). `cmd/cron/billing-invoice`,
    `LockBillingInvoice`, exit codes and counters, depguard, `make build`, the
    `cron-billing-invoice` image in the release matrix, and `.env.example`.
13. **RPCs** (§9). `GetUpcomingInvoice`, `ListInvoices` and
    `RemovePaymentMethod`: proto, handlers, authz registry entries,
    `ResourceInvoice` and reasons.
14. **Operator invoice commands** (§12). `show --invoices` with the last
    error, `invoice void` refusing a covered row, and `invoice retry`.
15. **Docs fold-in** (§18.5). This file into `billing.md`, `payments.md` and
    `usage.md`, the sections §21 retires deleted, and the `CLAUDE.md` pointers.
16. **Migration 022** (§14). `provider_product_id`, in the release after 021's
    images are out.

Pull requests follow the seams: 2–5 for pricing and mandates, 6–11 for the
ledger, 12–15 for the pass, the surface and the docs, and 16 on its own.
Outside this repo: gitops (§18.3), the dashboard in `../app` (§18.4), and the
Dodo test-mode run (§18.2), which gates turning billing on, not merging.
