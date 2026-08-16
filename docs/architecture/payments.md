# Payments — Dodo

Design reference for the second billing slice: taking money. Entitlement — what
an org is *allowed* to send — is [`billing.md`](billing.md) and is already
implemented; counting is [`usage.md`](usage.md). This document covers the
provider, checkout, the webhook inbox, and the one question that decides the
shape of everything else: who owns the price.

> **Status: design, awaiting review. No code from this slice exists** — the one
> exception is §4, which was decided and applied to migration 019 before it
> merged, since it removes columns rather than adding any. §15 tracks what is
> decided and what is still open. An earlier, reviewed, all-at-once Dodo build
> exists at `archive/billing-2026-08-15`; §16 records where this design
> deliberately departs from it.

---

## 1. Scope

**In:** Dodo Payments as merchant of record; self-serve checkout for the three
paid tiers; a signed webhook inbox; the provider-reported states entitlement
cannot derive (`PAST_DUE`, `CANCELLED`); a reconcile pass; cancellation on org
delete; negotiated deals billed through Dodo like everything else, bought from
the pug dashboard rather than from a link we send (§5.2).

**Out:** cheques, wire transfers, and any payment that happens outside Dodo
(§16 — this is the largest departure from the archived build); overage charges;
per-seat pricing; multi-currency (§3); an in-dashboard plan switcher (Dodo's
customer portal does that); revenue reporting.

**Unchanged:** `Resolve`, the quota window, the plan catalog's quotas, and every
existing test. `billing_entitlements` gains exactly one nullable column (§6) and
`pug billing set` one flag. This slice is
additive. If it were reverted, orgs would keep resolving exactly as they do
today.

## 2. Structural invariants

1. **Pug owns the quota; Dodo owns the money.** `included_events` is pug's
   number and changing it changes what an org may send. The amount charged is
   Dodo's, and pug cannot alter it by writing to Postgres.
2. **One writer per row.** The operator writes `billing_entitlements`. The
   webhook writes `billing_subscriptions`. Neither writes the other's table.
   This is what makes drift structurally impossible rather than a thing to
   remember (§4).
3. **Every paid org has a Dodo subscription**, negotiated deals included. There
   is no manual-payment path, so "entitlement with no live subscription" is a
   reconcilable defect rather than a legitimate state.
4. **The webhook fails closed.** No secret configured ⇒ the route is not
   mounted and Dodo gets 404s. Never verify-nothing.
5. **Enforcement stays out.** Nothing here blocks ingestion. A `PAST_DUE` org
   keeps sending events (§11), and that is deliberate.

## 3. Locked decision: USD only

Dodo is a merchant of record and can present local currency to a buyer, but pug
sells in USD and stores USD, and every plan in the catalog is USD today.

This is enforced rather than assumed, in exactly one place: a subscription whose
currency is not `USD` is **rejected at the webhook boundary** — stored in the
inbox, marked processed, logged as an error, and not applied. A currency pug
cannot render honestly must not silently become a number on a dashboard.

The payoff for writing the guard down instead of leaving it implicit: while it
holds, `price_cents` is an accurate field name. When multi-currency arrives, the
guard is the single place that changes, and the rename to `price_minor_units`
happens with it — JPY has no cents, so the name and the constraint fall together
or not at all.

## 4. Who owns the price

This is the decision the rest of the slice hangs off, so it is stated before the
mechanics.

An earlier cut of the entitlement slice let an operator type `--price 40000
--currency USD` into `pug billing set` and stored it. Nothing charged anybody, so
the number was a note. The moment Dodo bills the org, that same number becomes a
**second authority on what the customer pays** — and the two can disagree the first time a deal is repriced in
Dodo's dashboard. The dashboard would then render a stale figure as fact, which
is worse than rendering nothing.

**Decided: pug stores no money at all.** The columns below were dropped from
migration 019 before it merged; what follows is why, kept because the question
comes back every time somebody wants a price on a page.

| Question | Answer | Source |
|---|---|---|
| What may this org send? | `included_events` | pug — catalog, or the org's override |
| What does this org pay? | the amount | Dodo — the subscription, mirrored read-only |
| What is the list price of a tier? | `price_cents` | pug — the Go catalog, a marketing number |

Concretely (**done**): `price_cents_override` and `currency_override` are gone
from migration 019, `--price` / `--currency` are gone from `pug billing set`, and
`ErrPriceNeedsCurrency`, `ErrCurrencyNeedsPrice` and `isCurrencyCode` went with
them. What a deal was
agreed at goes in `--note`, which is already there and is honest about being a
record rather than a source of truth.

`GetBillingStatus`'s `price_cents` field survives unchanged — for a standard
tier it is the catalog price, and for an org with a live subscription it is the
subscription's amount. The RPC surface does not change; only the authorship
does.

What this buys, beyond one fewer thing to keep in sync: the price/currency pair
constraint, its asymmetry, the CLI's two guards, and the `price_minor_units`
naming question all stop existing.

**The counter-argument, recorded:** a comped or annual deal has an agreed
amount an operator may want stored where a query can reach it. `--note` is
prose. If that turns out to matter, the column comes back — as a nullable
mirror written by the webhook, never by a person.

## 5. Custom deals

**A negotiated deal is a Dodo product plus a pug entitlement row.** It cannot
live only in pug: Dodo charges products, so a deal Dodo has never heard of is a
deal nobody can pay for. What can live only in pug is the *quota*, and it does.

The split for `custom`:

- **In Dodo:** a product at the negotiated monthly price, and the subscription
  the customer holds against it. Created **by hand in Dodo's dashboard**, not by
  pug (§5.1).
- **In pug:** `plan_slug = 'custom'` with `included_events_override` — the quota
  Dodo has no concept of — plus that product's id
  (`provider_product_id`, §5.2), `contract_ends_at` and `--note`.
- **The join:** the subscription row the webhook writes, attributed by the
  `metadata.org_id` pug sets on the checkout session it opens.

Note what is *not* here: no price typed into pug, and no payment link emailed to
the customer. The operator pastes one product id; the customer buys from the pug
dashboard.

### 5.1 Why the product is created by hand

The archived build had `CreateProduct` and a `pug billing checkout create`
command that minted a bespoke product and a first checkout URL over the API.
This design drops both for v1.

At $10–$30 list tiers, custom deals will be rare and each one already involves a
human conversation. Creating the product in Dodo's dashboard takes a minute, is
where the operator can see tax categories and the product collection anyway, and
removes an entire API surface — product creation, repricing, the
create-then-checkout race, and the "what if the product exists but the
subscription never activated" state — from pug.

What pug keeps is the part a dashboard cannot do: the quota, and the link back to
the org. Add `CreateProduct` when the deal rate makes the paste annoying, not
before.

Worth being explicit, because it looks like a contradiction with §4: creating the
product over the API would mean passing the negotiated price *through* pug, which
is not the same as storing it — the price would be an argument to Dodo and the
product id would be what comes back. §4 is not what rules `CreateProduct` out;
the API surface it drags in is.

### 5.2 The first payment, without sending a link

A custom product is not in the catalog, so nothing generic can offer it. Rather
than emailing a payment link, the operator records the product id on the org and
lets the dashboard do the rest:

```
pug billing set o_2f9k --plan custom --events 5000000 \
                       --provider-product prod_2f9k... \
                       --name "Acme Enterprise" --actor "praveen/INV-123"
```

`billing_entitlements` gains one nullable `provider_product_id`, operator-written
like every other column on that table (invariant 2). `GetBillingStatus` reports
whether one is set; when it is, the dashboard shows this org — and only this org
— a buy button, and `CreateCheckoutSession` opens a Dodo checkout against that
product with `metadata.org_id` set. On `subscription.active` the webhook writes
the subscription row and the deal is live.

So the whole flow is: create the product in Dodo, paste its id, tell the customer
it is waiting in their dashboard. No link leaves our hands, and no price enters
pug.

**A payment link still works** and stays the fallback — for a customer who wants
to pay before anyone touches pug, or a deal where the buyer is not a pug user at
all. The webhook attributes it the same way, provided `metadata.org_id` is set on
the link.

**The one ordering hazard:** if the operator never runs `pug billing set`, the
org holds a `custom` subscription with no quota row, and `custom` has no catalog
quota to fall back on. Resolution treats that as the free floor and the reconcile
pass reports it (§9) — a customer paying for nothing is exactly the kind of thing
that must be loud. Recording the product id first makes this the ordinary path
rather than a thing to remember, since the quota is written by the same command.

### 5.3 What the customer sees in the pug dashboard

A custom-deal customer does everything from the pug dashboard, like anybody else.

| Moment | Where the customer acts | Why |
|---|---|---|
| First payment | **Buy button in the pug dashboard** → Dodo checkout | The org's `provider_product_id` is what the button points at (§5.2) |
| Card update, invoices, receipts, cancellation | **"Manage billing" in the pug dashboard** → Dodo's customer portal | Checkout leaves a `provider_customer_id`, which is all `CreatePortalSession` needs (§12) |
| Their quota and usage | The pug dashboard, as today | Never left pug |

The only operator step is pasting a product id once per deal, and it happens
during a conversation that was going to happen anyway. Nothing about a custom
deal makes the customer's experience different from a self-serve one — they never
receive a link, and they never see Dodo except as the checkout and portal pages a
self-serve customer sees too.

## 6. Storage

One new table, one new inbox, and one new column on `billing_entitlements`:

```
billing_entitlements
  provider_product_id  text          -- nullable; the Dodo product a custom deal
                                     -- is bought against (section 5.2). Operator-
                                     -- written, like every other column here.
```

NULL is every org that is not a negotiated deal — the catalog tiers get their
product ids from config (section 16), not from the row.

```
billing_subscriptions
  org_id                char(20) primary key references orgs(id) on delete cascade
  provider              text not null default 'dodo'
  provider_customer_id  text not null
  provider_sub_id       text not null unique
  plan_slug             varchar(50) not null      -- resolved from the product id
  status                text not null             -- provider vocabulary, narrowed on read
  price_cents           bigint not null           -- mirror; §4
  currency              varchar(3) not null       -- always USD while §3 holds
  current_period_start  timestamptz
  current_period_end    timestamptz
  provider_updated_at   timestamptz not null      -- the CAS column; §8
  create_time           timestamptz not null default now()
  update_time           timestamptz not null default now()
```

`org_id` as the primary key keeps "one subscription per org" structural, the
same reasoning as the entitlement row. `provider_sub_id` is unique so a webhook
cannot attach one Dodo subscription to two orgs.

```
billing_webhook_deliveries
  webhook_id     text primary key      -- Dodo's, so a retry collides
  event_type     text not null
  payload        jsonb not null
  received_at    timestamptz not null default now()
  processed_at   timestamptz           -- null ⇒ died mid-apply, retry may re-apply
  error          text not null default ''
```

The inbox is kept because it is the only thing that makes Dodo's retries safe
(§8), and because a payload that failed to apply is otherwise unrecoverable.

## 7. Resolution with a subscription

`Resolve` grows one input. The order, most specific first:

1. A **live subscription** (`active`, `on_hold`, `past_due`) supplies the plan.
2. An **operator grant** (`billing_entitlements.plan_slug`, non-floor) supplies
   the plan when there is no live subscription — this is how a comped deal and a
   pre-paid grant keep working with no money involved.
3. Overrides from the entitlement row apply on top of either, which is what
   makes a `custom` subscription's quota come from pug.
4. Otherwise the derived trial, then free, exactly as today.

A `cancelled` or `expired` subscription supplies nothing and the org falls to
whatever is beneath it — usually free, because the trial is long past. The row
is kept rather than deleted: "when did this lapse" is a question support asks.

Every existing rule in [`billing.md`](billing.md) §6 survives unchanged. An org
with no subscription row resolves exactly as it does today, which is what makes
this slice revertible.

## 8. The webhook

Endpoint `POST /webhooks/dodo` (and `/webhooks/dodo/`), mounted with
`mux.Handle` **directly** — never the `handle()` closure in `server.go`, which
registers into the authz contract and would fail `assertServedServicesMatch` at
startup. `/mcp` is the precedent.

A Connect RPC is the wrong shape, not merely extra ceremony: verification needs
the exact raw bytes and Connect hands the handler a decoded message; the payload
schema is Dodo's and evolves without us, so protovalidate would 400 valid
deliveries into a retry loop; and the caller authenticates by HMAC, which none
of the four auth modes represent. So the handler sits outside the interceptor
chain and does its own signature check, its own body cap
(`http.MaxBytesReader`, 1 MiB), and its own `telemetry.RecordError`.

Verification is [Standard Webhooks](https://www.standardwebhooks.com/):
HMAC-SHA256 over `"{webhook-id}.{webhook-timestamp}.{raw body}"`, constant-time
compared against **each** space-delimited signature in `webhook-signature` (the
header carries several during secret rotation), ±5 minute timestamp tolerance.
The raw body must be read before any JSON decode — re-serializing changes the
bytes and breaks the signature.

Dodo's delivery semantics, and the code each one forces:

| Guarantee | Consequence |
|---|---|
| 8 retries, exponential backoff, 15s timeout | Persist to the inbox and apply inline; return 2xx only once the row is durable. |
| **No ordering guarantee**; each delivery carries the *latest* payload | CAS on `provider_updated_at` = the `webhook-timestamp` header — apply only when `>=` the stored value. Delivery time bounds payload freshness precisely *because* every delivery carries the latest object, and it is uniform across event types, which no payload field is. |
| Retries reuse `webhook-id` | On primary-key conflict, **re-apply when `processed_at is null`**. A bare conflict→200 would neutralize Dodo's retry of a delivery that died mid-apply. |
| New event types appear over time | Unknown types are stored, marked processed, ignored. A type we do not handle must never 500 and never retry forever. |

Handled: `subscription.active`, `.updated`, `.renewed`, `.on_hold`, `.failed`,
`.cancelled`, `.expired`, `.plan_changed`. Every `subscription.*` runs one apply
path and the new state comes from the payload's `status`, not the event name —
the name only selects side effects. `payment.*` and `refund.*` are stored and
ignored in this slice; the ledger is the third slice
([`billing.md`](billing.md) §11.3).

Attribution is `metadata.org_id`, set on every checkout and every custom product
link, falling back to `provider_customer_id`. A delivery that resolves to no org
is stored, marked processed, and logged — never applied to a guess.

## 9. Reconcile

A `pug cron billing-reconcile` pass, shaped like `pug cron usage`: one-shot,
advisory-locked, non-zero exit on failure, run by a k8s CronJob. It is the
backstop for the one thing the inbox cannot cover — a webhook that never arrived
at all.

For every org with a subscription row, fetch the subscription and apply the same
CAS. Then two consistency reports, which are the point of invariant 3:

- A subscription Dodo says is live that pug has no row for.
- An entitlement granting a paid or `custom` plan with no live subscription
  behind it — including the §5.2 case of a paid custom deal with no quota.

Both are logged and counted, not auto-fixed. An automatic repair here would be
writing to the money side of the system from a guess.

## 10. Deleting an org must cancel first

The `on delete cascade` drops both rows and Dodo knows nothing about it, so a
deleted customer keeps being charged — a refund and a chargeback, not merely an
inconsistency. Org deletion cancels at the provider **before** the local delete,
and refuses to proceed if the cancel call fails. This is the one place in the
slice where a provider error blocks a user-initiated action, deliberately.

## 11. Dunning

A failed renewal changes nothing about entitlement. `PAST_DUE` keeps the quota;
degrading a paying customer's product over an expired card is worse for both
sides than a few unbilled days. Dodo retries and emails on its own schedule, and
`subscription.cancelled` is what finally drops the org to the floor — by then
the customer has had every notice Dodo sends.

## 12. RPC surface

Two additions to `dashboard.billing.v1.BillingService`, both JWT, both
**admin-only**. `authz.ResourceBilling` already exists and is granted to the
viewer floor for the read; these need a new admin-only action on it, because the
quota banner stays on the viewer floor but starting a checkout is spending money:

- `CreateCheckoutSession(plan_slug) → checkout_url`. For `custom` it checks out
  against the org's own `provider_product_id` and returns `FailedPrecondition`
  when none is recorded; for a catalog tier it uses the configured product id.
- `CreatePortalSession() → portal_url`, `FailedPrecondition` for an org with no
  `provider_customer_id` — trialing, free and comped orgs have never checked out.

`GetBillingStatus` gains `subscription_status`, `current_period_end` and a
`purchasable` bool (true for a catalog tier, or for `custom` once the org has a
`provider_product_id`) — the last is what the FE renders the buy button from,
without ever seeing a product id. It keeps the viewer floor. Plan changes and cancellation go through Dodo's customer
portal; no `ChangePlan` RPC in this slice.

## 13. Configuration

| Var | Default | Meaning |
|---|---|---|
| `PUG_BILLING_ENABLED` | `false` | Unchanged. Off ⇒ no quota, and the checkout RPCs return `Unavailable`. |
| `PUG_DODO_API_KEY` | — | Absent ⇒ no provider. Checkout returns `Unavailable`; everything else works. |
| `PUG_DODO_ENVIRONMENT` | `test` | `test` or `live`. A malformed value fails startup. |
| `PUG_DODO_WEBHOOK_SECRET` | — | Absent ⇒ the route is **not mounted** (invariant 4). Billing enabled with a key but no secret WARNs at startup. |

Billing enabled with no Dodo credentials is a supported mode, not a broken one:
quotas, grants and comped deals all work, and only the buy button is missing.
That is the self-hosted configuration.

## 14. Testing

`internal/core/billing` already has `TestMain` and no `t.Parallel()`
([`CLAUDE.md`](../../CLAUDE.md) § Testing); the additions follow. The provider is
an interface (`billing.PaymentProvider`) with a fake in tests — no container
talks to Dodo. What must be tested against Postgres: the CAS on out-of-order
deliveries, the retry that re-applies an unprocessed row, and resolution with
every (subscription status × entitlement) pair from §7.

Signature verification gets a table test with a known-good vector, a tampered
body, a stale timestamp and a multi-signature header. This is the code most
likely to be wrong in a way nothing else catches.

## 15. Decisions

1. **§4 — pug stores no money.** DECIDED yes, and already applied to migration
   019 and the CLI.
2. **§3 — USD only**, enforced at the webhook boundary. DECIDED.
3. **§5.1 — custom products are created by hand in Dodo**, not over the API.
   DECIDED for v1.
4. **§5.2 — no payment links.** The operator pastes the product id onto the org
   and the customer buys from the pug dashboard. DECIDED; a link stays a working
   fallback rather than the normal path.
5. **§12 — is checkout admin-only?** Recommended yes. OPEN.
6. **§10 — does a failed provider cancel block org deletion**, or does it proceed
   and log loudly? Recommended block. OPEN.

## 16. Divergences from the archived build

`archive/billing-2026-08-15` is a reviewed, working implementation of roughly
this slice plus the next two. Where this design differs, it is on purpose:

- **No payments outside Dodo.** The archive made `provider = NONE | DODO |
  MANUAL` first-class, with a `pug billing payment record` command and a wire
  transfer path. Dropped: every deal goes through Dodo, which is what makes
  invariant 3 and the §9 reconcile report meaningful. This deletes a table, a
  command and a resolution branch.
- **No `CreateProduct` / `pug billing checkout create`** (§5.1). The bespoke
  product is created in Dodo's dashboard and its id pasted into `pug billing set`,
  which gets the customer a buy button without pug touching the product API.
- **The plan catalog stays in Go.** The archive moved plans to rows because a
  purchasable tier binds to a per-environment product id. This design maps
  product id → slug in config instead: three tiers, two environments, six
  strings, which does not justify migrating the catalog out of code. Revisit
  when tiers are edited by someone who cannot deploy.
- **Custom deals are pug quotas, not `PRIVATE` plan rows.** The archive modelled
  a deal as a private plan; entitlement overrides already do this, and they
  shipped.
- **No payment ledger in this slice.** `payment.*` deliveries are stored and
  ignored until the third slice.

The archive remains the reference for §8's mechanics — its delivery-semantics
table is reproduced here nearly verbatim because it was derived from Dodo's
documented behaviour and reviewed once already.
