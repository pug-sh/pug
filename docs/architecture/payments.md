# Payments

Design reference for the second billing slice: taking money. Entitlement — what
an org is *allowed* to send — is [`billing.md`](billing.md) and is already
implemented; counting is [`usage.md`](usage.md). This document covers the
provider, checkout, the webhook inbox, and the one question that decides the
shape of everything else: who owns the price.

**Dodo Payments is the provider, and it sits behind an interface (§2.1).** The
provider is a choice pug should be able to change without touching resolution,
the inbox, the reconcile pass or the dashboard — merchant-of-record terms and
fee schedules move, and being locked to one is a commercial risk, not only a
technical one. Dodo is the first and, for now, the only implementation.

> **Status: implemented 2026-09-06.** §17 records where the build departs from
> this design, and §15's open questions are resolved there. An earlier,
> reviewed, all-at-once Dodo build exists at `archive/billing-2026-08-15`; §16
> records where this design deliberately departs from it. Provider-swappability
> (§2.1) was added to this design on 2026-09-06 and is not reflected in the
> archive at all.

---

## 1. Scope

**In:** a merchant-of-record provider behind an interface (§2.1), implemented
once for Dodo Payments; self-serve checkout for the three paid tiers; a signed
webhook inbox; the provider-reported states entitlement cannot derive
(`PAST_DUE`, `CANCELLED`); a reconcile pass; negotiated deals billed through the provider like everything else, bought from
the pug dashboard rather than from a link we send (§5.2).

**Out:** cheques, wire transfers, and any payment that happens outside the
provider (§16 — this is the largest departure from the archived build); **a
second provider implementation** (§2.1); overage charges;
per-seat pricing; multi-currency (§3); an in-dashboard plan switcher (Dodo's
customer portal does that); revenue reporting.

**Unchanged:** the quota window, the plan catalog's quotas, and every existing
test. `Resolve` gains one argument — a nullable subscription (§7) — and every
branch it already has survives beneath it. `billing_entitlements` gains exactly
one nullable column (§6) and `pug billing set` one flag. This slice is
additive. If it were reverted, orgs would keep resolving exactly as they do
today.

## 2. Structural invariants

1. **Pug owns the quota; the provider owns the money.** `included_events` is
   pug's number and changing it changes what an org may send. The amount charged
   is the provider's, and pug cannot alter it by writing to Postgres.
2. **One writer per row.** The operator writes `billing_entitlements`. The
   payments side writes `billing_subscriptions` — the webhook, reconcile and
   `ConfirmCheckout`, all through the one CAS in `applySubscription`, so there is
   no second notion of "newer". Neither side writes the other's table. This is
   what makes drift structurally impossible rather than a thing to remember (§4).
   The two sides still meet on one org: a custom subscription takes its quota
   from the entitlement row, so `applySubscription` takes the same
   `pg_advisory_xact_lock` a `billing clear` does and re-reads that row inside
   it. Otherwise a clear can commit between the mapping and the write, leaving a
   live custom subscription against no row — the free floor, and exactly the
   stranding `Clear`'s own guard refuses to cause.
3. **Every paid org has a provider subscription**, negotiated deals included.
   There is no manual-payment path, so "entitlement with no live subscription"
   is a reconcilable defect rather than a legitimate state.
4. **The webhook fails closed.** No secret configured ⇒ the route is not
   mounted and the provider gets 404s. Never verify-nothing.
5. **Enforcement stays out.** Nothing here blocks ingestion. A `PAST_DUE` org
   keeps sending events (§11), and that is deliberate.
6. **Nothing above the seam knows the provider's name.** Resolution, the inbox,
   reconcile, the RPCs and the dashboard operate on pug's own vocabulary. The
   provider's names, payload shapes and signature scheme stop at §2.1's
   boundary.

### 2.1 The provider seam

The requirement is to be able to change merchant of record without rewriting the
slice. The way to get that is a **narrow interface at the boundary**, not a
generic payments abstraction: the second is how you end up with a
lowest-common-denominator layer that fits no provider well and still needs a
rewrite.

Everything that genuinely differs between merchants of record is small and lives
at the edges:

| Behind the interface (per provider) | Above it (pug's, written once) |
|---|---|
| Signature scheme and header names | The delivery inbox and its retry semantics (§8) |
| Event-type names and payload shape | The CAS on freshness, attribution, product → slug mapping |
| Status vocabulary (§7's mapping table) | pug's own status vocabulary and `Resolve` (§7) |
| Checkout / portal session creation | The RPCs that call them (§12) |
| Subscription fetch | The reconcile pass (§9) |
| Product id namespace | `billing_subscriptions`, `billing_webhook_deliveries` (§6) |

```go
// billing.PaymentProvider is the whole seam. Two verbs for the inbound half,
// four for the outbound; nothing else in the slice imports a provider package.
type PaymentProvider interface {
    Name() string

    // Verify authenticates a raw delivery. It takes the exact bytes because
    // every scheme signs those, not a decoded message.
    Verify(headers http.Header, rawBody []byte) (Delivery, error)
    // Normalize maps one verified delivery onto pug's vocabulary. Returning a
    // zero SubscriptionEvent means "store, mark processed, ignore".
    Normalize(Delivery) (SubscriptionEvent, error)

    CreateCheckoutSession(ctx context.Context, in CheckoutInput) (string, error)
    CreatePortalSession(ctx context.Context, customerID string) (string, error)
    FetchSubscription(ctx context.Context, subID string) (SubscriptionEvent, error)
    CancelSubscription(ctx context.Context, subID string) error
}
```

Three rules keep the seam from rotting:

- **The payload is never abstracted.** `billing_webhook_deliveries.payload`
  stores the bytes as sent; `Normalize` produces one small canonical struct and
  everything downstream reads only that. A common payload schema across
  providers is the thing that cannot be maintained.
- **The canonical status vocabulary is pug's** (§7) and does not grow to fit a
  provider. A state pug has no word for normalizes to "not live", which is the
  safe direction — see §7.
- **One implementation, not two.** Writing a second provider now to prove the
  abstraction costs more than it saves and validates nothing: the interface is
  shaped by the second real provider, not by a speculative one. What this slice
  ships is the seam and Dodo behind it.

**Why one provider still needs the seam now, not later:** the two things that
make a provider swap expensive are not the API calls, they are the *stored*
shapes — a subscription row keyed to one provider's assumptions, and a payload
column read directly by the apply path. Both are decided in this slice and both
are hard to change afterwards, so §6 pays for them now (a `provider` column, a
key that permits two providers at once) while they are free.

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
| How long is its history kept? | `retention_days` | pug — catalog, or the org's override |
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
  Dodo has no concept of — and `retention_days_override` when the deal names a
  retention, plus that product's id
  (`provider_product_id`, §5.2), `contract_ends_at` and `--note`.
- **The join:** the subscription row the webhook writes, attributed by the
  `checkout_ref` pug mints for the checkout session it opens — or, for a payment
  link, by `metadata.org_id` paired with the `provider_product_id` staged above.

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

```shell
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
all. It carries no `checkout_ref`, so it attributes on `metadata.org_id` **paired
with** the `provider_product_id` already on that org's row: `pug billing set
--provider-product` has to run *before* the link is paid, not after. That pairing
is what keeps a buyer-settable `metadata_org_id` from placing a subscription on
someone else's org — the worst a forged one can do is buy the org exactly the
product an operator already staged for it.

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

Two new tables, one new inbox, and one new column on `billing_entitlements`:

```text
billing_entitlements
  provider_product_id  text          -- nullable; the Dodo product a custom deal
                                     -- is bought against (section 5.2). Operator-
                                     -- written, like every other column here.
```

NULL is every org that is not a negotiated deal — the catalog tiers get their
product ids from config (section 13), not from the row: one key per purchasable
tier, mapping a catalog slug to a provider product id.

The id belongs to whichever provider is configured, and a provider swap
invalidates every stored one along with every config key. That is a re-paste per
custom deal at cutover, which at the expected deal rate (section 5.1) is minutes
of work — recorded here so it is a known cost rather than a surprise. It is not
worth a `provider` column beside it: an id that names the wrong provider is
caught by the checkout call failing, immediately and loudly, on the one org it
affects.

```text
billing_subscriptions
  id                    char(20) primary key
  org_id                char(20) not null references orgs(id) on delete cascade
  provider              text not null             -- no default; §2.1
  provider_customer_id  text not null
  provider_sub_id       text not null
  plan_slug             varchar(50) not null      -- resolved from the product id
  status                text not null             -- pug's vocabulary; §7
  provider_status       text not null             -- the provider's, verbatim; support reads this
  price_cents           bigint not null           -- mirror; §4
  currency              varchar(3) not null       -- always USD while §3 holds
  current_period_start  timestamptz
  current_period_end    timestamptz
  provider_updated_at   timestamptz not null      -- the CAS column; §8
  create_time           timestamptz not null default now()
  update_time           timestamptz not null default now()

  unique (provider, provider_sub_id)
  unique (org_id) where status in ('active', 'past_due')   -- partial index
```

**One LIVE subscription per org, not one row per org.** An earlier cut of this
design made `org_id` the primary key, the same reasoning as the entitlement row.
That is wrong the moment the provider is swappable: a cutover cannot be atomic —
the customer re-checks-out on the new provider while the old subscription is
still winding down — so for a period an org legitimately holds two rows, one
live and one cancelled. A single-row-per-org key makes that migration
impossible to perform without deleting billing history, which is the record you
least want to destroy during exactly that operation.

The partial unique index keeps the invariant that actually matters — an org
cannot be *billed twice* — while permitting the dead row beside the live one. It
is also what makes §9's reconcile able to report "two live subscriptions" as the
database refusing it rather than as a report nobody reads.

`(provider, provider_sub_id)` rather than a bare unique on the id: two providers
can mint the same opaque string, and the pair is what a delivery is actually
attributed by. A surrogate `id` primary key because the natural key is now
composite and the history rows reference it.

```text
billing_webhook_deliveries
  provider       text not null
  webhook_id     text not null         -- the provider's, so its retry collides
  event_type     text not null
  payload        jsonb not null
  received_at    timestamptz not null default now()
  processed_at   timestamptz           -- null ⇒ died mid-apply, retry may re-apply
  error          text not null default ''

  primary key (provider, webhook_id)
```

Keyed by the pair, not the id alone: the id is the provider's namespace, and two
providers running side by side during a cutover (§6, `billing_subscriptions`)
could otherwise have one's delivery silently swallowed as the other's retry.

The inbox is kept because it is the only thing that makes the provider's retries
safe (§8), and because a payload that failed to apply is otherwise
unrecoverable.

`payload` is the delivery as sent, which for a subscription event carries the
customer's name, email, billing address and any tax id — personal data pug does
not otherwise store. Replay needs the bytes, so the controls are on the row
rather than on the fields: no RPC or MCP tool reads this table, and the
reconcile pass prunes rows **90 days** after `processed_at`, or after
`received_at` for one that never processed -- the provider's retries are spent
long before that, so an undecodable body does not keep its payload for good.
Retention is
therefore the whole control today: `compliance` erases a data subject, not an
org, and pug has no org-deletion path at all. An erasure query was written and
removed — it matched deliveries through `billing_subscriptions`, which cascades
away with the org, so it erased nothing once the org was gone. Anything
long-lived that a query wants — status, period, amount — belongs on
`billing_subscriptions`, which outlives the payload it came from.

## 7. Resolution with a subscription

`Resolve` grows one input: a `*Subscription` beside `rec Record`, nil for the
orgs that have none. A separate argument rather than a field on `Record` because
the two rows have different writers (invariant 2) and different readers — a
`Record` is what `pug billing` produces, and folding a webhook-owned row into it
would put the operator's type in the provider's write path.

The order, most specific first:

1. A **live subscription** supplies the plan.
2. An **operator grant** (`billing_entitlements.plan_slug`, non-floor) supplies
   the plan when there is no live subscription — this is how a comped deal and a
   pre-paid grant keep working with no money involved.
3. Overrides from the entitlement row apply on top of either, which is what
   makes a `custom` subscription's quota come from pug.
4. Otherwise the derived trial, then free, exactly as today.

"Live" is pug's word, not the provider's. `Normalize` (§2.1) maps on write and
both columns are stored (§6), so this rule is one comparison against a
vocabulary we control and support can still see what the provider actually said.

pug's vocabulary is fixed and does not grow to accommodate a provider:
`active`, `past_due`, `paused`, `cancelled`, `expired`, `failed`. Dodo's mapping
is the first implementation of it:

| Dodo `status` | pug `status` | Live |
|---|---|---|
| `active` | `active` | yes |
| `on_hold` | `past_due` | yes — the card failed, the entitlement does not (§11) |
| `paused` | `paused` | no |
| `cancelled`, `expired`, `failed` | same word | no |
| anything else | stored verbatim, treated as not live | no |

The last row is the rule that makes a new provider safe to add: a state pug has
no word for is **not live**, so an unmapped status can only ever withhold a
plan, never grant one. A provider mapping is therefore allowed to be incomplete
on its first day and fails in the direction that costs a support ticket rather
than free service.

`subscription.paused` and `.unpaused` run the same apply path as every other
`subscription.*` — the payload's status is what moves the row, so pausing drops
the org to whatever is beneath the subscription and unpausing restores it, with
no event-name-specific branch. An unrecognized status is not live: a state we
have never seen must not silently grant a plan.

A not-live subscription supplies nothing and the org falls to whatever is beneath
it — usually free, because the trial is long past. The row is kept rather than
deleted: "when did this lapse" is a question support asks.

Every existing rule in [`billing.md`](billing.md) §6 survives unchanged. An org
with no subscription row resolves exactly as it does today, which is what makes
this slice revertible.

## 8. The webhook

Endpoint `POST /billing/webhooks/<provider>` — `/billing/webhooks/dodo` and
`/billing/webhooks/dodo/` today — mounted with `mux.Handle` **directly**, never
the `handle()` closure in `server.go`, which registers into the authz contract
and would fail `assertServedServicesMatch` at startup. `/mcp` is the precedent.
The path is scoped under `/billing/` rather than a bare `/webhooks/` so the
generic root stays free for the next subsystem that needs an inbox.

**The path carries the provider name deliberately.** Sniffing the provider from
the headers would mean trying each verifier in turn, which is both a signature
oracle and unresolvable when two schemes share a header name. It also leaves
room for a cutover to mount two routes and retire the first on its own schedule,
with no flag day -- room, not today's wiring: only the constructed provider's
route mounts (§13), so the second one is a code change rather than a config
change.

A Connect RPC is the wrong shape, not merely extra ceremony: verification needs
the exact raw bytes and Connect hands the handler a decoded message; the payload
schema is the provider's and evolves without us, so protovalidate would 400
valid deliveries into a retry loop; and the caller authenticates by HMAC, which
none of the four auth modes represent. So the handler sits outside the
interceptor chain and does its own body cap (`http.MaxBytesReader`, 1 MiB) and
its own `telemetry.RecordError`, and delegates the signature check to
`PaymentProvider.Verify` (§2.1).

The route handler itself is provider-agnostic and is written once: cap the body,
read the raw bytes, `Verify`, insert into the inbox, `Normalize`, apply, mark
processed. Only `Verify` and `Normalize` change per provider.

Dodo's verification is [Standard Webhooks](https://www.standardwebhooks.com/):
HMAC-SHA256 over `"{webhook-id}.{webhook-timestamp}.{raw body}"`, constant-time
compared against **each** space-delimited signature in `webhook-signature` (the
header carries several during secret rotation), 24h timestamp tolerance — wide
because a retry reuses its original timestamp, so a tighter window would reject
every late attempt.
The raw body must be read before any JSON decode — re-serializing changes the
bytes and breaks the signature. Not every provider uses this scheme, which is
why it lives behind `Verify` rather than in the handler.

Dodo's delivery semantics, and the code each one forces. These are the weakest
guarantees pug should assume of any provider — a provider that promises ordering
simply makes the CAS a no-op, so the inbox shape survives a swap unchanged:

| Guarantee | Consequence |
|---|---|
| 8 retries, exponential backoff, 15s timeout | Persist to the inbox and apply inline; return 2xx only once the row is durable. |
| **No ordering guarantee**; each delivery carries the *latest* payload | CAS on `provider_updated_at` = the `webhook-timestamp` header — apply only when strictly newer than the stored value, **or** equal while the stored row is still live. Delivery time bounds payload freshness precisely *because* every delivery carries the latest object, and it is uniform across event types, which no payload field is. The header is whole seconds, so a cutover's cancellation and activation can tie; the tie-break resolves toward withholding, so an equal stamp can end a subscription but never revive one. |
| Retries reuse `webhook-id` | On primary-key conflict, **re-apply when `processed_at is null`**. A bare conflict→200 would neutralize Dodo's retry of a delivery that died mid-apply. |
| New event types appear over time | Unknown types are stored, marked processed, ignored. A type we do not handle must never 500 and never retry forever. |
| The payload's **shape** changes under us | A body `Normalize` cannot decode is the one unapplicable-looking case that IS retried: a redeploy inside the retry window fixes it, and marking it processed would consume the delivery, losing the replay. |

Handled: `subscription.active`, `.updated`, `.renewed`, `.on_hold`, `.failed`,
`.cancelled`, `.expired`, `.plan_changed`. Every `subscription.*` runs one apply
path and the new state comes from the payload's `status`, not the event name —
the name only selects side effects. That is also what makes the apply path
portable: event-type vocabularies differ sharply between providers, subscription
*states* barely do, so `Normalize` maps names to nothing and status to
everything. `payment.*` and `refund.*` are stored and
ignored in this slice; the ledger is a later one
([`billing.md`](billing.md) §11.4).

Attribution is a `checkout_ref` — 32 crypto-random bytes pug mints and stores in
`billing_checkout_sessions` (§6) *before* it opens the checkout, then sets in the
checkout's metadata. A row there is keyed by the ref, carries the org and the
provider, and is pruned on the inbox's 90-day window: past it the ref has either
produced a subscription row, which carries attribution from then on, or belonged
to an abandoned checkout. It falls back to `metadata.org_id`, but **only** when that
org's `provider_product_id` equals the delivery's product (the staged-deal link
below), and then to `provider_customer_id` — that one only while the customer
names a single org. `metadata.org_id` is never sufficient alone: Dodo's static
payment links accept `metadata_*` query parameters, so a buyer can set it to any
org id, and trusting it would let anyone who pays put a subscription on an org
they do not belong to. Checkout sets `always_create_new_customer`, so one
buyer paying for two orgs gets two provider customers rather than one shared
between them; without that flag Dodo matches on email and the fallback would
resolve to two orgs, and the portal would open on whichever it found. A delivery that resolves
to no org, or to two, is stored, marked processed, and logged — never applied to
a guess.

**A body that verifies and will not decode answers 500, not 401.** The signature
passed, so it is pug's payload mapping that is behind, not an unauthenticated
caller: `ErrUndecodable` separates the two so the delivery is retried and recorded
rather than filed under the warning a port scanner produces.

**A product pug cannot place is the same case, in the granting direction only.**
If `product_id` maps to no configured tier (§13) and does not match the attributed
org's `provider_product_id`, there is no `plan_slug` to write, so the delivery is
stored, marked processed, logged as an error, and **not applied** — the same
disposition as a foreign currency (§3) and an unattributable delivery, for the
same reason: retrying cannot fix a mapping that lives in config, and 8 retries
would only delay the alert. Every such delivery is counted and logged by the
reconcile pass as `Rejected` (§9) — the subscription walk cannot see it, because a
delivery pug did not apply wrote no subscription row at all.

**A delivery that ENDS a subscription is exempt.** A non-live status keeps the
`plan_slug` already stored on the row rather than resolving one from the product,
because refusing a cancellation whose product has left the config would leave the
row `active` and the org on a tier it stopped paying for — an unmapped product may
withhold a plan, never preserve one. Only a delivery with no stored row to end
falls back to the refusal above. Reconcile (§9) reports it as a third inconsistency —
a live subscription against a product pug does not know — which is the signal
that a deploy is missing a product key or that an operator created a product
without pasting its id.

## 9. Reconcile

A `cmd/cron/billing-reconcile` pass, shaped like `pug cron usage`: one-shot,
advisory-locked, non-zero exit on failure, run by a k8s CronJob. It is a
standalone binary, not a `pug cron` subcommand. It is the
backstop for the one thing the inbox cannot cover — a webhook that never arrived
at all.

For every org with a subscription row, fetch the subscription through
`FetchSubscription` (§2.1) and apply the same CAS. During a cutover the pass
runs per provider — a row names the provider it belongs to (§6), so an org with
a winding-down subscription and a live new one is reconciled against the right
API for each. Then the consistency reports, which are the point of invariant 3:

- A subscription pug stores that the provider no longer knows — a 404 on the
  re-read, or a row left behind by an environment switch. A finding, deliberately
  not a read failure: counting it as one would hold the CronJob red on every
  later run over a row that is never coming back.
- An entitlement granting a paid or `custom` plan with no live subscription
  behind it. An org with **no** entitlement row is invisible here, since the query
  reads `billing_entitlements`; the §5.2 case of a paid custom deal with no quota
  row surfaces as the unmappable-product finding below instead.
- A live subscription against a product no config key and no org row maps to
  (§8), which is a delivery that could not be applied.
- Two live subscriptions for one org, refused by the partial unique index. The
  one finding here that means an org may be paying twice.
- A live subscription no writer can store: an unsold currency, no status, no
  customer, a negative price. The writer names it (`ErrSubscriptionUnapplicable`)
  rather than reporting a skip, or it would be neither an apply nor a finding and
  the pass would print a sweep it did not make.

All are logged and counted, not auto-fixed. An automatic repair here would be
writing to the money side of the system from a guess. A pass that could not read
the provider — or could not read or write Postgres either side of it — exits
non-zero; the rest are findings for a person. So does a pass that names a
provider with no API key configured: reconciling nothing and exiting 0 is
indistinguishable from a healthy run, and this is the backstop for a webhook that
never arrived.

## 10. Deleting an org must cancel first

**Not built — pug has no org-deletion path yet.** When it gets one: the `on
delete cascade` drops both rows and the provider knows nothing about it, so a
deleted customer keeps being charged — a refund and a chargeback, not merely an
inconsistency. Org deletion must cancel at the provider **before** the local
delete, and refuse to proceed if the cancel call fails.

## 11. Dunning

A failed renewal changes nothing about entitlement. `PAST_DUE` keeps the quota;
degrading a paying customer's product over an expired card is worse for both
sides than a few unbilled days. The provider retries and emails on its own
schedule, and the delivery that normalizes to `cancelled` is what finally drops
the org to the floor — by then the customer has had every notice the provider
sends. This policy is pug's and survives a provider change; only the dunning
schedule behind it moves.

## 12. RPC surface

Four additions to `dashboard.billing.v1.BillingService`, all JWT. `ListPlans`
is a read and stays on the viewer floor; the three that spend money are
**admin-only** through a new `ActionCreate` on `authz.ResourceBilling`, because
the quota banner stays on the viewer floor but starting a checkout does not:

- `CreateCheckoutSession(plan_slug) → checkout_url`. For `custom` it checks out
  against the org's own `provider_product_id` and returns `FailedPrecondition`
  when none is recorded; for a catalog tier it uses the configured product id.
- `CreatePortalSession() → portal_url`, `FailedPrecondition` for an org with no
  `provider_customer_id` — trialing, free and comped orgs have never checked out.
- `ConfirmCheckout(session_id) → confirmed` (§12.1), which is how a returning
  buyer is confirmed without a delivery having arrived.

`GetBillingStatus` gains `subscription_status`, `current_period_end` and a
`purchasable` bool — what the FE renders the buy button from, without ever
seeing a product id. It is true only when the checkout would actually open:
billing enabled, a Dodo API key configured, and a product to check out against
(a configured catalog tier, or `custom` once the org has a
`provider_product_id`). It gates the buy button as a whole; whether a particular
tier can be bought is `PlanOption.purchasable`, which does share a helper with
`CreateCheckoutSession`'s refusal — a button that cannot work is worse than no
button. Tested both
configured and unconfigured. It keeps the viewer floor. Plan changes and cancellation go through Dodo's customer
portal; no `ChangePlan` RPC in this slice.

### 12.1 Confirming the buyer who came back

The webhook is the authority for the subscription *lifecycle* — renewal,
dunning, cancellation, expiry, refunds — because none of those has a redirect to
be carried on. It is a bad authority for the one moment somebody is watching.

`CreateCheckoutSession` therefore also returns the provider's `session_id`, and
`ConfirmCheckout(org_id, session_id)` re-reads that checkout **straight from the
provider** and applies it through `applySubscription` — the same CAS the webhook
and reconcile write through, so no third notion of "newer" exists.
For Dodo the walk is session → payment → subscription (`FetchCheckoutOutcome`),
because the session status carries a payment id but no subscription id, and only
the subscription object carries the product, price, period and metadata. The last
hop is `FetchSubscription` itself, which is what keeps the event the same shape
as the one a delivery normalizes to.

What this buys is not latency, it is **reachability**. Before it, the dashboard's
return handler polled `GetBillingStatus` — pug's own state, which nothing but a
delivery changes — so the checkout moment depended on inbound connectivity. On a
deployment whose webhook URL is not reachable (self-hosted behind NAT, or a
laptop) a purchase could never complete: the poll ran its ~17.5s and told a
paying customer their payment was "still confirming", forever.

Five rules make it safe:

- **`session_id` is a claim, not evidence.** The subscription it resolves to must
  carry the `metadata.org_id` pug wrote at checkout, and it must equal the
  caller's org — `PermissionDenied` otherwise. `metadata.org_id` alone only
  *names* an org, though (§8: static payment links let a buyer set `metadata_*`
  from the URL), so a subscription carrying a `checkout_ref` must also resolve
  that ref against `billing_checkout_sessions` to the same org — a ref pug never
  minted, or minted for another org, is refused. There is deliberately **no
  fallback to attribution by customer id** here, unlike §8's webhook path: nobody
  chose which delivery arrived, but the caller chose this id. Being caller-chosen
  is also why the field is `pattern`-constrained to the provider's own id
  alphabet: it is echoed into the provider's URL path, where a `../` would address
  a different endpoint with pug's API key.
- **A session the provider does not have is a dead checkout, not a fault.** An
  unknown or aged-out id comes back as `ErrCheckoutFailed`, so a bookmarked or
  probed `session_id` tells the buyer their checkout did not complete instead of
  polling a 500 forever and recording an exception on the money path each time.
- **`confirmed` is read from the provider's state, not from whether the write
  landed.** The CAS also skips the write when a delivery already stored something
  newer, and reporting that as "not confirmed" would push a buyer who already
  holds the plan into the poll this exists to remove. The one refusal that is
  *not* a harmless skip — a second live subscription hitting
  `billing_subscriptions_one_live_idx` — comes back as `ErrTwoLiveSubscriptions`
  rather than as a `false` from the writer, or a buyer would be told "confirmed"
  for a row that was never stored.
- **A subscription pug has no word for is written but not confirmed.** `pending`
  stores its row — which is what leaves the org a customer to manage — and grants
  nothing, so telling the buyer it worked would be a lie.
- **Every paid-but-unplaceable case is returned, not swallowed.** A foreign
  currency (§3), an unmappable product, a second live subscription and a
  subscription carrying no status at all are the dispositions §8 stores and
  alerts on, but here somebody is waiting, so each surfaces as
  `FailedPrecondition` under its own reason and the dashboard says the payment
  needs a person rather than that the page will update shortly. None may fall
  through to the pre-payment `BILLING_NOT_PURCHASABLE`, which says the plan
  "cannot be purchased" to somebody who just bought it.
- **A checkout the provider has given up on is a refusal, not a wait.**
  `FetchCheckoutOutcome` reads the payment's status, so a declined or cancelled
  card returns `ErrCheckoutFailed` → `BILLING_CHECKOUT_FAILED` instead of the
  zero event that means "not settled yet". Without it a decline is
  indistinguishable from a slow payment and the buyer polls for money that will
  never arrive. Anything else — `processing`, an unfinished 3DS challenge, a word
  Dodo adds later — stays "not yet", so an unknown state can only delay the
  answer, never invent a failure.

The client keeps the poll as its own fallback: `confirmed=false` is the ordinary
"not settled yet" for a buyer who beat their own payment home, and the webhook is
still coming.

## 13. Configuration

| Var | Default | Meaning |
|---|---|---|
| `PUG_BILLING_ENABLED` | `false` | Unchanged. Off ⇒ no quota, and the checkout RPCs return `Unavailable`. |
| `PUG_BILLING_PROVIDER` | `""` | Which provider to construct: `dodo` today. Empty ⇒ no provider at all. An unrecognized value **fails startup** rather than silently disabling checkout, since the two are indistinguishable from the dashboard. |
| `PUG_DODO_API_KEY` | — | Absent ⇒ no provider. Checkout returns `Unavailable`; everything else works. |
| `PUG_DODO_ENVIRONMENT` | `test` | `test` or `live`. A malformed value fails startup. |
| `PUG_DODO_WEBHOOK_SECRET` | — | Absent ⇒ the route is **not mounted** (invariant 4). Billing enabled with a key but no secret WARNs at startup. |
| `PUG_DODO_PRODUCT_<SLUG>` | — | One per purchasable catalog tier (`..._STARTER`, `..._GROWTH`, `..._SCALE`), mapping the slug to a Dodo product id. Both directions: checkout reads slug → product, the webhook reads product → slug (§8). A tier with no key is not purchasable (§12); `custom` has no key, since its product id lives on the org's row. |

Provider credentials stay under their own `PUG_<PROVIDER>_` prefix rather than a
generic `PUG_PAYMENTS_*`: a second provider's keys then sit beside the first's
instead of overwriting them, which is what lets both be configured at once
during a cutover. Only `PUG_BILLING_PROVIDER` decides which one is constructed
for checkout, and that one provider's webhook route is the only one mounted —
so a cutover must keep the outgoing provider configured until its last
cancellation has arrived.

Billing enabled with no provider credentials is a supported mode, not a broken
one: quotas, grants and comped deals all work, and only the buy button is
missing. That is the self-hosted configuration.

## 14. Testing

`internal/core/billing` already has `TestMain` and no `t.Parallel()`
([`CLAUDE.md`](../../CLAUDE.md) § Testing); the additions follow. The provider is
an interface (§2.1) with a fake in tests — no container talks to a payments API.
What must be tested against Postgres: the CAS on out-of-order deliveries, the
retry that re-applies an unprocessed row, every Dodo status in §7's table
reaching its normalized value, and resolution with every (subscription status ×
entitlement) pair from §7.

**The seam is tested by the fake, not by a second provider.** Everything above
§2.1 — inbox, CAS, attribution, resolution, reconcile — is exercised through a
fake provider whose `Verify` and `Normalize` are trivial, which is what proves
those paths hold no Dodo assumption. A test that the fake and Dodo agree on
anything beyond the interface would be testing the mock.

Signature verification gets a table test with a known-good vector, a tampered
body, a stale timestamp and a multi-signature header. This is the code most
likely to be wrong in a way nothing else catches, and it is per provider.

## 15. Decisions

1. **§2.1 — the provider sits behind an interface**, with Dodo as the only
   implementation and no second one written speculatively. DECIDED 2026-09-06.
   The two costs paid up front are in storage (§6): the subscription key permits
   an org to hold a dead and a live row at once, and both the inbox and the
   subscription row name their provider.
2. **§4 — pug stores no money.** DECIDED yes, and already applied to migration
   019 and the CLI.
3. **§3 — USD only**, enforced at the webhook boundary. DECIDED.
4. **§5.1 — custom products are created by hand in Dodo**, not over the API.
   DECIDED for v1.
5. **§5.2 — no payment links.** The operator pastes the product id onto the org
   and the customer buys from the pug dashboard. DECIDED; a link stays a working
   fallback rather than the normal path.
6. **§12 — is checkout admin-only?** DECIDED yes, as recommended. Granted as
   `ActionCreate` on `ResourceBilling` rather than a new verb: both session RPCs
   mint a provider session. The read stays on the viewer floor.
7. **§10 — does a failed provider cancel block org deletion?** MOOT, not
   decided. This codebase has no org-delete path at all — no RPC, no CLI, no
   `delete from orgs` query — so there is nothing to guard. See §17.

## 16. Divergences from the archived build

`archive/billing-2026-08-15` is a reviewed, working implementation of roughly
this slice plus the next two. Where this design differs, it is on purpose:

- **No payments outside the provider.** The archive made `provider = NONE |
  DODO | MANUAL` first-class, with a `pug billing payment record` command and a
  wire transfer path. Dropped: every deal goes through the provider, which is
  what makes invariant 3 and the §9 reconcile report meaningful. This deletes a
  table, a command and a resolution branch. Note the archive's `provider` column
  is a different idea from §2.1's — it enumerated *payment methods*, one of
  which was "none", where this one names which merchant of record a row came
  from.
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


## 17. What the build changed

Six departures from the design above, all made during implementation.

**`CancelSubscription` is not on the interface (§2.1, §10).** Its only caller
would be org deletion, and pug has no org-delete path: `OrgsService` serves no
Delete, and no query deletes an org. Shipping the verb now would be the
speculative abstraction §2.1 argues against. When org deletion lands, it adds the
method and the cancel-before-delete rule together — and that is the moment to
settle §15.7.

**`status` is not constrained to pug's six words (§6, §7).** The design's §7
table says an unmapped provider state is "stored verbatim, treated as not live",
which a `check (status in (...))` makes impossible. Refusing to store it instead
leaves the row on its LAST KNOWN status — for a lapsing subscription, that grants
a plan nobody is paying for, the exact opposite of the safe direction. So the
column is checked non-empty, the provider's own word is stored, and
`ParseSubStatus` refusing it at read time is what makes it not live.

**`GetBillingStatus` reports `manageable` as well as `purchasable` (§12).** The
portal's precondition is a provider *customer*, which a cancelled org still has
while reporting `SUBSCRIPTION_STATUS_UNSPECIFIED` — so a client inferring the
button from the status hides invoices from the org most likely to want them.
Same rule as `purchasable`: read from the one helper the RPC refuses on. Writing
it as `ProviderCustomerID != ""` on the resolved entitlement reproduced exactly
the drift this is meant to prevent, and a test caught it.

**A lapsed contract no longer gates the overrides when a subscription is live
(§7).** `applyOverrides` was contract-gated, which collapsed a live custom deal
to no quota — and then to the free floor — on the day its agreed term passed,
while Dodo went on charging. The contract bounds a grant an operator made; it
cannot expire a subscription the provider still says is live.

**`ListPlans` was added (§12).** The design adds two RPCs and says the dashboard
renders its buy button from `purchasable` — but `CreateCheckoutSession` takes a
plan *slug*, so something has to name the tiers, and the catalog is Go rather
than rows precisely so it is not queryable. The alternative was a hardcoded
catalog in the frontend, which would put a second authority on what a plan costs
— the mistake §4 exists to prevent, one layer up. It sits on the viewer floor
beside `GetBillingStatus`: a price is a marketing number, and the person reading
the quota banner is the one who wants to know what the next tier costs. It never
returns a product id, never the floors, and offers `custom` only to the org whose
row records its product.

**`ConfirmCheckout` was added, and the redirect stopped being a spinner (§12.1).**
The design has the dashboard poll after checkout, and the implementation did —
but it polled pug's own state, which only a delivery changes, so it verified
nothing and made the checkout moment depend on a reachable webhook URL. Found by
testing locally, where there is none: the payment succeeded at Dodo and the
dashboard read "Trial" indefinitely. The reconcile pass is not a
recovery for it either — it walks `ListBillingSubscriptionsByProvider`, so with
no stored row there is nothing to re-read.

`provider_product_id` is also on `billing_entitlement_history`, which §6 does not
mention: the history is a snapshot of the row, and "who pasted this product id,
and when" is a support question about the one field that decides whether an org
can spend money.
