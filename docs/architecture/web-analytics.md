# Web Analytics

> **Status: CODE COMPLETE, NOT SHIPPED (2026-07-17).** Written and green in dev/CI; **nothing here
> is committed, and migrations 008–010 have NOT been applied to production** — the deploy runbook
> below is pending, not history. Includes the review amendments recorded inline (timezone gate,
> panel scopes, locale normalization, picker widening, UTM column completion in the 008 mutation,
> mutation↔Derive parity test). The resolved
> open-questions log is at the bottom. Defines which event properties get promoted to dedicated
> ClickHouse columns and which become rollup dimensions so pug can serve a privacy-first
> web-analytics dashboard. Companion to [`insights.md`](insights.md),
> [`clickhouse.md`](clickhouse.md), [`ingestion.md`](ingestion.md).

## Goal and serving model

Target experience: a stat row — **Users, Sessions, Pageviews,
Pages/session, Bounce rate, Session duration** — over a bucketed timeseries, plus breakdown panels:
**Pages, Entry/Exit pages, Referrers, Channels, Countries/Regions/Cities, Languages,
Browsers/OS/Devices (+versions), Screen sizes, UTM ×5, Events**. Every panel is a single-dimension,
initially-unfiltered query — exactly the shape the existing rollup fast path serves. Clicking a row
adds a filter, and filtered queries already fall back to the raw path, which promoted columns keep
fast. Sub-day granularity (an hourly "today" view) and realtime are raw-path by design (the rollups
are day-keyed/session-keyed).

Two serving-model caveats, stated once here: **(a) every rollup fast path requires a
UTC-midnight-aligned window, and the *bucketed* ones additionally require UTC bucketing** — a
non-UTC `timezone` sends trends/segmentation and the session panels to the raw path, which the
promoted columns keep fast. Top K (the Pages/Countries/…/UTM row and Events list below) does not
bucket by time, so `topKQueryForExecution` deliberately does not consult `timezone`; the
day-alignment guard is the whole gate there. **(b) the session-grain panels (the stat
row's Sessions/Bounce/Duration/Pages-per-session and Entry/Exit/Referrers/Channels) should scope
`kind=page_view`** — that makes bounce = single-pageview session (rather than single-event, which
any background or custom event would silently un-bounce) and computes entry states over pageviews
only, so a URL-less first event can't claim a session's first-touch attribution.

Every panel maps onto the existing insights machinery — no new query engine, no new insight type:

| Panel | Query | Serving path |
|---|---|---|
| Users / Pageviews (+timeseries) | trends `kind=page_view`, UNIQUE_USERS / TOTAL | event rollup `$__total__` |
| Sessions / Bounce / Duration | session SESSIONS / BOUNCE_RATE / AVG_DURATION | session rollup |
| Pages/session | session **AVG_EVENTS_PER_SESSION** (new), scope `kind=page_view` | session rollup |
| Pages, Countries, Regions, Cities, Languages, Browsers, OS, Devices, Versions, Screens, UTM ×5 | top K over the dimension | event rollup |
| Entry / Exit pages | session ENTRY / EXIT, breakdown `$pathname` | session rollup |
| Referrers / Channels | session SESSIONS, breakdown `$referrerDomain` / `$channel` | session rollup (first-touch; see [Channel semantics](#channel-semantics-session-grain-not-event-grain)) |
| Events list | top K, DIMENSION_EVENT_KIND | event rollup |
| Hourly buckets, realtime, any filtered view | same specs | raw path (columns make it cheap) |

## Property model

### Promoted columns (migration 008): 16 → 26

New auto-properties and their dedicated `events` columns. "Derived" means computed server-side by
the new `internal/attribution` enricher (see below); "SDK" means client-sent (the seeder already
emitted `$referrer`/`$locale`/`$screenWidth`/`$screenHeight`/`$pageTitle`; it gained
`$utmTerm`/`$utmContent` and now routes everything through the same `attribution.Derive`).

| Property | Column | Type | Source | Overwrite policy |
|---|---|---|---|---|
| `$pathname` | `pathname` | `String` | derived: `url.Parse($url).Path` (decoded, not `EscapedPath` — so the SQL mirror's `decodeURLComponent(path(...))` can reproduce it), empty → `/` | derive-if-absent (SDK may send logical/route paths) |
| `$hostname` | `hostname` | `LowCardinality(String)` | derived: URL host, lowercased, port stripped | derive-if-absent |
| `$referrer` | `referrer` | `String` | SDK (`document.referrer`) | as sent, never derived |
| `$referrerDomain` | `referrer_domain` | `LowCardinality(String)` | derived: referrer host, lowercased, one leading `www.` stripped; **blanked on self-referral** (equals own hostname after `www.`-strip) | **always server-derived** (client value stripped first, like `$bot_score`) |
| `$channel` | `channel` | `LowCardinality(String)` | derived: rule table over (referrerDomain, `$utmSource`, `$utmMedium`); only when `$url` present | **always server-derived** |
| `$locale` | `locale` | `LowCardinality(String)` | SDK | casing/separator normalized before storage (`en-us`/`en_US` → `en-US`) — rollup rows are permanent, so variants must never fragment the Languages panel |
| `$screenSize` | `screen_size` | `LowCardinality(String)` | derived: `"{w}x{h}"` when `$screenWidth`/`$screenHeight` both parse > 0 | derive-if-absent |
| `$utmTerm` | `utm_term` | `LowCardinality(String)` | SDK, or parsed from `$url` query | only-if-absent |
| `$utmContent` | `utm_content` | `LowCardinality(String)` | SDK, or parsed from `$url` query | only-if-absent |
| `$pageTitle` | `page_title` | `String` | SDK | as sent |

Also new behavior on **existing** UTM properties: `$utmSource`/`$utmMedium`/`$utmCampaign` are
completed from `$url` query params (`utm_source=`…) when the SDK didn't send them — removes any SDK
dependency for UTM. (The 008 mutation applies the same completion to the historical
`utm_source`/`utm_medium`/`utm_campaign` columns, guarded if-absent, so history classifies channel
identically to the live enricher.)

All ten are `PromotedString` in `promotedAutoColumns`. LowCardinality follows the `utm_campaign`
precedent (arbitrary-but-bounded tenant text); `pathname`/`referrer`/`page_title` stay plain
`String` (genuinely high cardinality, like `url`/`city`). `utm_term` is the most aggressive LC
choice (freeform paid keywords) — accepted under the same precedent, revisit if part dictionaries
bloat.

### Event-rollup dimensions (migration 009): 10 → 20

`materializedDims` (`internal/core/insights/rollup.go`) and the `ARRAY JOIN` in the rollup MV gain:

```
$pathname  $hostname  $referrerDomain  $channel  $locale
$screenSize  $utmTerm  $utmContent  $browserVersion  $osVersion
```

`$browserVersion`/`$osVersion` are already promoted columns — they only join the dim list. Caveat,
accepted: an unfiltered versions panel shows bare majors ("126") mixed across browsers; the
sharper view (filter `$browser = Chrome`, then break down) goes raw anyway. MV insert amplification
goes 11 → 21 rows/event (EAV is linear per dim; post-merge storage = Σ per-dim daily cardinality —
dominated by `$pathname`, bounded by site page count).

### Session-rollup dimensions (migration 010): 11 → 16

`sessionMaterializedDims` (`internal/core/insights/session_rollup.go`) gains entry/exit
`argMin`/`argMax` state pairs for:

```
$pathname  $referrerDomain  $channel  $utmTerm  $utmContent
```

(22 → 32 state columns.) `$pathname` powers Entry/Exit pages; the rest power first-touch
acquisition panels.

### Deliberate exclusions

- `$url`, `$referrer` as **event-rollup** dims: high cardinality, superseded by
  `$pathname`/`$referrerDomain`. Both stay promoted for fast raw-path drill-downs
  (`$url` remains a session dim, as today).
- `$pageTitle` as a rollup dim: high-cardinality free text (≤1024 chars); the Pages panel keys on
  pathname. Promotion alone makes occasional title breakdowns fast on the raw path.
- Numeric `$screenWidth`/`$screenHeight` columns: the map + numeric filter projection already
  works; `$screenSize` is the display dimension.
- `$hostname`/`$locale`/`$screenSize` from the **session** rollup: not session-attribution
  dimensions; add later if demand appears.

### New session metric

`SESSION_METRIC_AVG_EVENTS_PER_SESSION` — `avg(event_count)` over session rows; with scope
`kind=page_view` this is **pages per session**. Rollup-eligible via the existing
`event_count_state`; raw form `avg(event_count)` over `buildSessionRowsCTE`. A first-class metric
rather than an FE division of two tiles because pageviews are occur-day-windowed while sessions are
keyed on session start and never clipped — dividing tiles disagrees visibly on boundary days.

## Derivation: `internal/attribution`

New leaf package (siblings: `internal/geo`, `internal/useragent`), stdlib-only (`net/url`,
`strings`), so `internal/core/clickhouse`, the ingest handler, and the seeder can all import it.

- **Owns the canonical `Prop*` constants** for `$url`, `$referrer`, UTM ×5, and every derived key
  above; `promoted_auto.go` swaps its string literals for them (same pattern as `geo.PropCountry`).
- **`Derive(Input) Output`** is a pure function (no providers, no ctx): URL decomposition, referrer
  domain + self-referral blanking, UTM extraction, screen-size formatting, channel classification.
  The seeder calls the same function so demo data and production classify identically.
- **Handler wiring**: `enrichAttribution` appended after `enrichVerifiedBot` in the SDK events
  handler chain (header-independent, per-event). Per-key overwrite policy per the table above;
  server-only keys (`$channel`, `$referrerDomain`) are deleted from client input first, mirroring
  the bot enrichers.
- **URL rules**: derive only from parseable http(s) URLs with a non-empty host (no fabricated `/`
  for garbage input); `$channel` is gated on the same validity (a merely-present garbage `$url`
  derives no channel); referrer accepts any scheme with a host (`android-app://…` → Referral);
  `$url` itself is never mutated; no path normalization in v1 (case/trailing-slash normalization is
  lossy).

### Channel taxonomy (normative)

First match wins; inputs normalized (`src`/`med` lowercased, `ref` = post-self-blank referrer
domain, suffix-matched against the domain sets so `l.facebook.com` matches `facebook.com`):

| # | Rule | Channel |
|---|---|---|
| 1 | paid medium (`^(.*cp.*\|ppc\|retargeting\|paid.*)$`) AND search source/ref | `Paid Search` |
| 2 | paid medium AND social source/ref | `Paid Social` |
| 3 | paid medium AND video source/ref | `Paid Video` |
| 4 | `med ∈ {display, banner, expandable, interstitial, cpm}` | `Display` |
| 5 | paid medium, unmatched source | `Paid Other` |
| 6 | search source/ref, or `med = organic` | `Organic Search` |
| 7 | social source/ref, or `med ∈ {social, social-network, social-media, sm}` | `Organic Social` |
| 8 | video source/ref, or `med = video` | `Organic Video` |
| 9 | `src or med ∈ {email, e-mail, e_mail, newsletter}` | `Email` |
| 10 | `med = affiliate` | `Affiliate` |
| 11 | `ref != ""` | `Referral` |
| 12 | any UTM present, or a referrer sent but unresolvable, yet unclassifiable | `Unassigned` |
| 13 | otherwise | `Direct` |

Domain sets live as Go consts in `internal/attribution/channel.go` (search: google + ccTLDs
matched structurally by a TLD-shaped-tail check rather than enumeration, bing, duckduckgo, yahoo,
baidu, yandex, ecosia, brave, startpage, perplexity…; social: facebook, instagram, x/twitter,
t.co, linkedin, tiktok, pinterest, reddit, threads, bsky, news.ycombinator…; video: youtube,
youtu.be, vimeo, twitch, dailymotion…). That file is the **single normative taxonomy** — this satisfies
`profiles.md`'s "no ad hoc channel" rule by defining the stable derivation it demanded; the
profile API still exposes no channel field.

Known refinement candidate: a referrer of `mail.google.com` (Gmail web) suffix-matches the google
search family and classifies **Organic Search**; arguably it should be Email or Referral. The
structural matcher already keeps `android-app://com.google.android.gm` (Gmail app) a Referral —
its `google.android.gm` tail is not TLD-shaped. Left verbatim per the reviewed table; an
exceptions set is a one-line taxonomy change if the Organic Search bucket looks inflated.

### Channel semantics: session-grain, not event-grain

Only a session's **landing** event carries the external referrer; mid-session pageviews have a
self-referrer (blanked) and usually no UTM, so per-event classification degrades to `Direct`
mid-session. (The direction flips for SPAs whose route changes keep `document.referrer` pinned to
the external referrer — confirm the browser SDK's virtual-pageview referrer behavior; the
session-grain panels are correct either way.) Therefore the Referrers and Channels **panels must
be session-grain** (`SESSIONS` + entry-state breakdown = first-touch attribution). The event-grain
`$channel` dim still exists in the event rollup — it is what makes the property discoverable in
the picker and supports kind-scoped exploration — but any event-grain channel widget carries this
caveat. Resolved: v1 keeps `Direct` (not an `Internal` bucket) for self-referred mid-session
events — the session-grain panels make the distinction invisible, and `Internal` would need the
pre-blank referrer signal threaded through the rule table; revisit if event-grain channel widgets
get real use.

**Known asymmetry (accepted): `referrer_domain` without `channel`.** `$referrerDomain` derives
from any referrer with a host, but `$channel` derives **only when a valid http(s) `$url` is
present** (`Derive`). So an event with a non-web `$url` (a native/hybrid app sending
`capacitor://…`/`myapp://…`) but a resolvable `$referrer` stores a `referrer_domain` with an empty
`channel` — `sum(channel) < sum(referrer_domain)` on such traffic. This is deliberate: a channel
for a non-web event would be a guess, and `Unassigned` is not obviously better than absent. The
event is still visible in the Referrers panel; it is simply absent from Channels. A spike in this
shape (a `$url` present but nothing derived) is observable via the
`events.attribution_derive_degraded_total` counter (`reason=url_not_web`). Revisit only if a real
tenant needs channel attribution for non-web events.

## Migrations, backfill, and live-deploy safety

The app is live (since 2026-07-13), so all three migrations are written for a hot system. Three
backfills are required — history would otherwise render as `''` in every new panel — and each uses
a different pattern because each has a different merge hazard:

1. **008 — columns + derivation backfill.** `ALTER TABLE events ADD COLUMN … DEFAULT ''` ×10
   (metadata-only, instant), then one `ALTER TABLE events UPDATE … SETTINGS mutations_sync = 2`
   (waits on every replica; equivalent to 1 on a single node) deriving values for pre-008 rows:
   pathname/hostname from the `url` column (ClickHouse `path()`,
   `domain()`/`domainWithoutWWW()`), referrer/locale/utm_term/utm_content/page_title from the
   residual `auto_properties` map keys (legitimate here — historical rows were never split),
   screen_size from the typed map slots, UTM column completion from the `url` query string,
   channel via a frozen nested-`multiIf` mirror of the Go rule table (one-shot, drift-safe;
   applies only to pre-deploy rows). The mutation is **not** merely an approximation where it can
   be exact: `TestIntegrationWebAnalytics/mutation_008_matches_attribution_derive` runs the
   migration file's own statement over a pre-008-style corpus (twice, pinning the idempotency guards) and
   asserts byte-equality with `attribution.Derive` — including `path('') → '/'`,
   `lower(domain())`, `+`-as-space query decoding, and the locale re-caser. Every assignment is
   guarded `if(col != '', col, <derived>)`, making the mutation **idempotent** and safe to re-run.
   The map residue is deliberately left in place — `PropertyExpr` reads columns for promoted keys,
   and the map-wins merge on read yields identical values; a cleanup mutation adds risk for zero
   benefit.
2. **009 — event rollup.** `ALTER TABLE dashboard_event_rollup_daily_mv MODIFY QUERY` with the
   21-tuple ARRAY JOIN — **never DROP→CREATE**, which would lose *all* dims (including
   `$__total__`) for events inserted in the gap; ClickHouse 26.5 (dev/test/prod) supports
   MODIFY QUERY on TO-table MVs (exercised in CI — the parity suites run the real migrations).
   Carries **no derivation mutation of its own** — 008's already ran in the same PreSync job, so
   009's backfill reads an already-derived `events` and a second copy would be a no-op full-table
   rewrite (see the deploy runbook). Followed by a **delta backfill INSERT restricted
   to the new `dim_name`s** — new EAV key rows are disjoint from every existing row, so there is
   no merge hazard and existing dims must NOT be re-inserted (a full-list backfill would double
   `cnt` for the old dims). The sub-second MODIFY→INSERT overlap can double-count new-dim `cnt`
   only (`uniq_state` is idempotent) — same accepted class as the documented redelivery
   over-count, noted in-file like migration 006 does; a cutoff cannot fully close it (an insert in
   flight during MODIFY is ambiguous either way).
3. **010 — session rollup.** `ADD COLUMN` ×10 `AggregateFunction(argMin/argMax, String,
   DateTime64(3))` states, `MODIFY QUERY` restating all 32 states, then a **partial-column
   backfill INSERT** listing only the key columns + the ten new states. Omitted
   `AggregateFunction` columns take their implicit default — the **empty aggregate state, which is
   the merge identity** — so `event_count_state`/`start`/`end` and existing entry/exit states are
   untouched and bounce/duration cannot double. This is the one backfill that is silently
   catastrophic if done naively (re-inserting `countState()` doubles every session's event count);
   the merge-identity behavior is pinned by
   `TestIntegrationWebAnalytics/session_rollup_partial_insert_merge_identity`, which re-runs the
   migration file's own backfill statement against a live session and asserts count/duration/entry-url are
   byte-stable. The argMin/argMax states themselves are idempotent under duplicate merge, so the
   MODIFY→INSERT overlap is harmless here.

**What needs no backfill:** the existing 10 event dims and 11 session dims (untouched), the
`property_keys` discovery tables, and profiles/aliases. Erasure needs no changes: the event rollup
is already erasure-exempt by documented decision (anonymous aggregates), and the session rollup is
deleted row-level by `session_id`, so new state columns ride along.

**Deploy runbook (single-step — one normal release).** Ship 008+009+010 and the binaries in one
gitops commit and sync once. Order is not a choice: `migrate-clickhouse.job.yaml` is an ArgoCD
`PreSync` hook running `pug clickhouse migrate` with no bound (`goose.UpContext` = all pending), so
every migration completes before the server/worker Deployments sync. That is also the required
order — the new binary's INSERT lists all 26 promoted columns and hard-fails on a pre-008 schema,
while the old binary tolerates 008 via the column DEFAULTs.

**Bump the `pug-migrate-clickhouse` digest in that same commit.** The migrations are *files* baked
into that image (`COPY --from=build /src/schema/clickhouse/migrations`, read from WORKDIR), not
embedded in the binary the server ships. Leave it pinned at its old digest while bumping
server/worker and the PreSync hook runs an image that has never heard of 008: it finds nothing
pending, **succeeds**, and then the new server meets a 16-column table with a 26-column INSERT —
every event ingest hard-fails, from a green sync. This is the one deploy step that fails silently
one stage before it detonates.

**Do NOT split this into two releases** (migrate 008 → deploy → migrate 009+010). Beyond the
PreSync hook not being able to express it, the middle window is broken: the deployed binary already
lists `$pathname` in `sessionMaterializedDims`, and `canUseSessionRollup` inspects only the
request — never the schema — so a session breakdown emits `argMinMerge(entry_pathname_state)`
against a column 010 has not created yet (`Code: 47 UNKNOWN_IDENTIFIER`), with no raw fallback. The
event rollup fails the same way but silently: `$locale` routes to `WHERE dim_name = '$locale'`,
finds no rows, and renders flat zero — a breakdown that works today on the raw path. Splitting is
strictly worse than the window it tries to avoid.

**Accepted cost, and its repair.** 009/010's MVs go live during PreSync; the rollout then takes
~30–90s (`maxSurge: 100%`, `maxUnavailable: 0`, `terminationGracePeriodSeconds: 30`), and old pods
keep ingesting throughout with no `enrichAttribution`. Those events land `''` in the new columns
*and* the live MV writes them `dim_value=''` rollup rows. Re-deriving `events` afterwards does not
fix the rollup — an MV fires only on INSERT — so the blank rows are permanent unless rebuilt.

**Repair (one procedure, covers every case).** Optional, and sized by the window's traffic rather
than the table: run migration 008's derivation mutation, then migration 009's `DELETE` of the new
`dim_name`s followed by its delta backfill INSERT. That rebuilds the new dims from `events`, the
source of truth, and is re-runnable by construction — the same property that makes 009 safe to
retry. This is deliberately the *only* recovery path: it covers the rollout window above, and it
covers a 009 retried after a long gap (old-binary rows landing between 008's mutation and the
backfill), so neither case needs a special case inside the migration. 009 therefore carries no
catch-up mutation of its own — a second copy of 008's mutation would be a guaranteed no-op
full-table rewrite on the PreSync path, and a second copy of the repo's hairiest SQL to keep in
sync, to automate a case a human is already present for.

## Accuracy & privacy

- **Redelivery caveats** extend to the new surfaces unchanged: rollup-served TOTAL/PER_USER_AVG
  over-count by the redelivery rate on new dims exactly as on old ones; the new
  AVG_EVENTS_PER_SESSION inflates under duplicates on the rollup path (countState) while the raw
  path self-corrects — same class as the documented BOUNCE_RATE caveat, pinned by a matching
  documentation test.
- **Privacy:** pathnames can embed user identifiers (e.g. `/users/jane`); they enter the
  erasure-exempt event rollup as anonymous aggregates with no per-person keys — the same accepted
  stance as `$city`/`$utmCampaign` today, now stated explicitly rather than implied. `$ip` handling
  is unchanged (stripped, never persisted). Visitor identity remains cookieless per the separate
  compliance 4.10 design; nothing here adds fingerprinting.

## Discovery / picker completeness

Promoted keys are stripped from the map at ingest, so the `property_keys` MV never observes them.
`mergePromotedAutoDimensions` now injects **all promoted string columns**
(`clickhouse.PromotedStringAutoProperties` — the twenty `materializedDims` plus `$url`,
`$referrer`, `$pageTitle`) into `GetFilterSchema`, with rollup-sourced counts where a dim exists
and count 0 otherwise; the dedup drop-predicate widened symmetrically so a frozen pre-promotion
`property_keys` entry can't duplicate an injected key. Still invisible by design: the
bool/numeric promoted keys (`$mobile`, `$bot_score`, `$verified_bot`) — typed injection would
need per-key value types (`promotedAutoDimValueType` is a blanket `String`) and stays out of
scope. → [`insights.md`](insights.md) (Property discovery).

## Testing strategy (as implemented)

- **Contract pins, repointed at the latest MV definition:** `TestMaterializedDimsMatchMigration`
  and `TestMigration009PromotedDimExprsMatch` parse 009's Up section (MODIFY QUERY block = full
  21-tuple ↔ `materializedDims`+`$__total__`; backfill block = `eventRollupDims009` exactly);
  `TestMigration006Frozen` / `TestMigration007Frozen` pin the shipped migrations to their
  historical content (guards against editing them). `TestMigration010SessionRollup
  {ColumnsMatchDims,DimExprsMatch}` do the same for the session rollup (old dims stated once in
  the MODIFY QUERY, new dims twice, backfill column list = key columns + new states exactly).
  The `auto_properties['$` ban applies to the 009/010 rollup blocks only — 008's mutation
  legitimately reads the map (historical rows were never split).
- **Mutation↔Derive parity:** `TestIntegrationWebAnalytics/mutation_008_matches_attribution_derive`
  inserts a pre-008-style corpus (map-resident keys, empty new columns), executes the migration
  file's OWN mutation statement twice (idempotency), and asserts exact equality with `attribution.Derive` on
  every derived column — the mutation's output becomes permanent rollup history via the 009/010
  backfills, so exactness matters beyond the channel `multiIf`.
- **Merge-identity gate for 010:**
  `TestIntegrationWebAnalytics/session_rollup_partial_insert_merge_identity` — full-state session
  via the live MV → re-run the migration file's own partial-column backfill → `countMerge`/duration/`entry_url`
  unchanged, new `argMin/MaxMerge` correct.
- **Parity:** `TestIntegrationWebAnalytics/rollup_parity_*` extend the rollup-vs-raw suites
  (trends/top-K on `$pathname`/`$channel`; ENTRY/EXIT on `$pathname`; SESSIONS by `$channel` and
  `$referrerDomain`; AVG_EVENTS_PER_SESSION trends + segmentation with a pages-per-session sanity
  value). These run the real migrations via testutil, so MODIFY QUERY itself is exercised in CI.
- **Unit:** `internal/attribution` table tests (URL edges, self-referral variants including the
  pinned not-collapsed subdomain case and the pinned `android-app://` Referral, channel rules
  row-by-row + precedence, UTM extraction, locale normalization, Derive purity); handler tests for
  the enricher (server-only strip + rederive, if-absent semantics, non-web events untouched);
  promoted-column lockstep tests (`TestWebAnalyticsPromotedKeysRoundTrip`,
  `TestPromotedColumnListsInLockstep`); picker-injection tests for promoted-but-not-rolled-up keys.

## Implementation map (written 2026-07-17; unshipped)

`internal/attribution/` (new: `attribution.go` Derive + Prop consts, `channel.go` normative
taxonomy, table tests) · `internal/core/clickhouse/promoted_auto.go` (+10 `PromotedString`
entries in lockstep: slice / `EventsInsertPromotedColumns` / `PromotedAutoRow` / `AppendArgs` /
`ScanDest` / merge helpers, plus `PromotedStringAutoProperties()` for the picker; +tests) ·
`internal/app/server/rpc/sdk/events/handler.go` (`enrichAttribution` appended after
`enrichVerifiedBot`; +tests) · migrations `008_add_web_analytics_columns.sql` /
`009_extend_dashboard_event_rollup.sql` / `010_extend_dashboard_session_rollup.sql` ·
`internal/core/insights/rollup.go` (`materializedDims` 10→20 via `eventRollupDims009`) +
`session_rollup.go` (`sessionMaterializedDims` 11→16 via `sessionRollupDims010`,
`sessionRollupDimSuffix`, `canUseSessionRollup` + AVG_EVENTS_PER_SESSION) + `builder.go`
(`sessionMetricAggExpr` = `if(count()=0, 0, avg(toFloat64(event_count)))`) + `service.go`
(picker widening) · `proto/shared/insights/v1/insights.proto`
(`SESSION_METRIC_AVG_EVENTS_PER_SESSION = 6`; only the enum NAME reaches the MCP input schema —
protoc-gen-mcp drops value comments, so model-facing semantics belong on the `Query` RPC's
leading comment, not here) ·
contract-test surgery per Testing strategy · seeder (`session.go`/`factory.go` route props
through `attribution.Derive` via `seed/clickhouse/attribution.go`; `catalog.go` gained
term/content + video/social/self-referral cases) · stale snake_case comment fixed in
`proto/common/events/v1/navigation_events.proto` · doc updates (`insights.md`, `clickhouse.md`,
`ingestion.md`, `profiles.md`, `CLAUDE.md`). Verified no-ops: erasure (event rollup exempt;
session rollup deleted row-level by `session_id`, new states ride along), protovalidate
(`defined_only` admits the new enum value), MCP tool wiring (proto-generated).

## Open questions — resolved

1. **`Internal` vs `Direct` for self-referred mid-session events** — keep `Direct` in v1; the
   session-grain panels make the distinction invisible and `Internal` needs the pre-blank signal
   threaded through. Documented in [Channel semantics](#channel-semantics-session-grain-not-event-grain).
2. **publicsuffix (eTLD+1) subdomain collapsing** — deferred; cross-subdomain traffic is
   genuinely ambiguous per tenant. The not-collapsed case is pinned in `attribution_test.go`.
3. **Hostname `www.`-normalization on the stored column** — stays as-sent; most sites 301 to one
   canonical host, and staging/app subdomains staying distinct is a feature.
4. **Locale normalization** — YES, shipped in v1 (`NormalizeLocale`, mirrored in the 008
   mutation): un-normalized values entering the rollup would fragment the Languages panel
   permanently.
5. **Picker widening** — YES for all promoted string columns
   (`clickhouse.PromotedStringAutoProperties` + a symmetrically widened dedup predicate in
   `mergePromotedAutoDimensions`); typed injection for the bool/numeric bot fields stays out of
   scope.
6. **"Web analytics" demo dashboard** — deferred: dashboard tiles can't render session insights
   yet, so a seeded board could only carry the event-rollup panels; revisit with the FE page.

## Out of scope

The FE web-analytics page itself (`../app` — the BE contracts it needs are exactly this design);
browser-SDK additions (confirm `@pug-sh/browser` sends `$referrer`/`$locale`/`$screenWidth`/
`$screenHeight`/`$pageTitle`; UTM becomes server-derived either way); per-tile bot-exclusion
toggles (works today as a filter → raw path). Cookieless visitor identity — formerly deferred here
as "compliance 4.10" — is now implemented (migration 011, `internal/cookieless`,
`include_cookieless`; → [ingestion.md](ingestion.md), [insights.md](insights.md)).
