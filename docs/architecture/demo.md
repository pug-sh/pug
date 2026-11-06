# Demo

Status: in progress. Built: the five boards, the generator properties they
read, six months of history, and the manual rebuild. Not built yet: the rest of
the generator and the frontend asks.

## Summary

The public demo (demo.pug.sh) is where most people see Pug first. Its job is to
show every capability Pug renders, on data that reads like a real shop that is
growing. Today it does neither.

This doc replaces the four demo dashboards with five new ones, fixes the demo data
so they look right, and rebuilds the production demo. The demo is down while it
backfills.

## The plan

**Nothing is edited in place.** The demo's events cannot be repaired. ClickHouse
holds them with random ids, and their history has the wrong shape (see "What is
wrong today"). The dashboards are seed code, not data. So the switchover deletes
the old project whole: its four boards, events, profiles and rollups. Then the
worker builds a new demo project from scratch.

**No migration.** The demo worker writes boards and events, as it does today.
Knowing which demo is current needs no new table either: it is the demo org's
oldest live project.

Steps:

1. **Generator.** Time-driven backfill, the story, the new properties (see "Data").
2. **Boards.** Replace the four definitions in
   `internal/app/seed/postgres/dashboards.go` with the five below.
3. **Rebuild.** Once the demo project is deleted, the worker gives the demo org a
   new project and backfills it. The automatic rebuild in "Rebuilding the demo in
   production" is not built.
4. **Tests.** Every tile renders data, the generator hits its rates, and a deleted
   demo project is replaced.
5. **App repo.** Lay split funnels out three to a row. It is the only FE change the
   boards need.
6. **Local preview.** You run `pug seed`. I screenshot all five boards in light and
   dark, and we iterate on the board code until they look right.
7. **Release** as usual: tag, then roll the new digests out, except the demo
   worker's, which waits for step 8. Rolled out on the old project, it would add
   the new boards beside the old ones, on old data.
8. **Switchover**, by hand at a quiet hour. Check that the demo org has no other
   live project, or the oldest one left becomes the demo. Delete the demo project
   (`pug projects delete d91cn26ml7kc7391f7eg --actor <you>`). Right away, roll
   the demo worker out with `PUG_DEMO_SEED_COUNT=4500000`, which keeps the
   backfill near the live rate of about 27k events a day; the old worker writes
   to the deleted project until it stops. The new worker creates a new project
   and backfills it. The demo is down until the project exists, then its boards
   fill in over roughly 10 to 20 minutes. If the backfill is interrupted, delete
   the new project the same way and restart the worker.
9. **Cleanup** is automatic. The purge cron erases the old project after
   deletion's usual one-hour wait.
10. **Verify.** demo.pug.sh shows the five boards. `DemoSignIn` returns a new
    project id. The old project's deletion reaches `done`.

Later refreshes repeat step 8 with the current demo project's id, the one
`DemoSignIn` returns. If the worker's image and settings are unchanged, restart it
instead of rolling it out.

## What is wrong today

Seen on demo.pug.sh and on a local seed, 2026-11-06.

| What you see | Why |
|---|---|
| The Revenue board is all green. | The FE colours a single-event chart by its event kind. Every commerce event (`add_to_cart`, `checkout_*`, `purchase`, `coupon_*`) is a shade of green. The board is five single-event commerce tiles plus a green threshold. |
| Hair-thin bars (UX Health, Acquisition, Cohorts). | Grouped bars with a breakdown at daily grain: 7 to 11 series over 30 to 180 days. Most bars sit near zero. |
| Series colours repeat. | The breakdown palette has 5 colours. Tiles split into 7 to 11 series. |
| "Net revenue" equals "Gross revenue". | Its filters (bots, `@pug.sh` emails) match no purchase. |
| Retention climbs from 15% to 65% over the weeks (production). | The backfill writes about 4k events a day. The live worker writes 5 to 8 times that. Cohorts from the backfill months "come back" more once live traffic starts. |
| Top customers have 115 orders in 180 days. | 10,000 users carry live-rate traffic. |
| Revenue is flat with two 5x towers. | There is no growth. A promo raises traffic and purchase share together. |
| Weekends spike to about 1.6x weekdays. Saturday is about 2x Tuesday. | `dayWeights` lifts Saturday to 1.7x Monday on purpose. The weekend hour table also carries about 20% more weight per day than the weekday tables, and the two multiply. |
| Crawlers count as people. GPTBot and SemrushBot are in the browser list. | The seeder never sets `$bot` or `$bot_reason`. |
| "Active users by platform" has one line. | It counts `page_view`, which only web sends. |
| Errors are web only. | Only the web journey emits `error_occurred`. |
| Top pages is mostly `/`. | Every web journey starts on the home page. |
| Revenue by campaign is mostly `(none)`. | The tile does not filter to sessions with a campaign. |
| Referrers show `https://` and the store itself. | The tile uses `$referrer`, not `$referrerDomain`. |
| Club trials convert 98%. Pushes get 63% clicks. | Both fall out of journey weights, not real rates. |
| Explainer text is cut off (Users board). | The cell is too short for its copy. |
| Money reads `666,169.91`. | No tile sets a number format. |
| Sankey, map and pie appear nowhere. | The seed predates them on dashboards. |

## Principles

1. **One question per tile, in the shop's words.** The title asks or answers it
   ("Which channels convert?"). The description names the Pug feature in one
   line.
2. **One hero per board, never repeated.** Across the boards, every chart Pug
   draws well appears at least once.
3. **A story you can read in five seconds.** Growth, a promo, a bad app release
   and its hotfix, the channel that converts best.
4. **Legible at a glance.** At most 5 series per chart (4 plus Others). Money in
   compact format. Only the cohort table scrolls.
5. **Mixed colours across each board.** A single-series chart takes its event's
   colour, so each board mixes hues: blue visitors, teal newsletter, red rage
   clicks, violet searches, gold members, pink push. Heroes are split and draw
   from the categorical palette. No board has more than two tiles in one hue.
6. **A balanced mix.** Each board uses at least six chart types, and at most one
   funnel, one pie and two ranked lists. About half the tiles show time (charts
   plus KPI sparklines). Flow charts (funnel, Sankey, cohorts) stay near one in
   seven.
7. **Web, iOS, Android and push on the same boards.** That is what sets Pug
   apart.
8. **It ages well.** Windows are relative to now, episodes recur, and live
   traffic matches the backfill, so the boards look the same next month.

## Charts

Every chart below was rendered on today's data before it went in.

| Chart | Tiles | Use it for | Avoid |
|---|---|---|---|
| KPI card | 8 | a headline number with its delta | |
| Lines | 3 | 2 to 5 series over time | 6 or more series |
| Area | 4 | one series with real volume, 50 or more events a day | a breakdown: it draws overlapping lines, not a stack |
| Column bars | 3 | one series, 30 days daily or any weekly board | 90 days daily at half width: the bars get thin |
| Stacked bars | 4 | a split over time, 3 to 5 parts | |
| Grouped bars | 1 | a weekly board, full width, 2 or 3 series | daily grain: hairlines, even at three series |
| Pie | 3 | 3 to 5 parts of a whole | more than one per board |
| Ranked list | 7 | a leaderboard (top K) | more than two per board: every top K draws the same list, whatever its view |
| Map | 3 | a geography that differs from the other maps | two maps with the same story: every map draws in the theme's one hue |
| Funnel | 4 | conversion, with step timing | see "Funnels" |
| Sankey | 1 | paths through the store | see "Sankey" |
| Cohort table | 1 | retention | anything but full width |
| **Total** | **42** | plus 5 explainers | |

The proto's `log_scale` is never used: the FE does not read it.

Today's breakdown palette has three vivid colours, then grey and black. A hero
split five ways therefore ends in grey and black. Pug's colourblind mode already
has six saturated hues, and the same charts look livelier in it (see "Frontend
asks").

Each board mixes seven or eight chart types, and its single-colour tiles each take
a different hue:

| Board | Chart types | Single-colour tiles |
|---|---|---|
| 1. Store at a glance | stacked bars, 4 KPIs, map, pie, area, lines, ranked list | Visitors per day (blue) |
| 2. Marketing & channels | lines, funnel, map, area, KPI, ranked list, stacked bars | Newsletter signups (teal) |
| 3. Shopper journeys | Sankey, area, funnel, column bars, 2 ranked lists, pie, KPI | Rage clicks (red), Searches per day (violet) |
| 4. Customers & loyalty | stacked bars, cohort table, grouped bars, funnel, column bars, lines, 2 ranked lists, KPI | New Club members (gold) |
| 5. Mobile app & push | stacked bars, column bars, KPI, pie, area, funnel, map, ranked list | Crashes per day (dark red), Push opens (pink) |

## Coverage

| Capability | Boards |
|---|---|
| Lines | 1, 2, 4 |
| Area | 1, 2, 3, 5 |
| Column, stacked and grouped bars | all |
| Pie | 1, 3, 5 |
| Map | 1, 2, 5 |
| KPI with prior-period delta, thresholds, compact and duration numbers | all |
| Top K by property, by user with profile; the Others row; a numeric average | all |
| Funnel, unsplit and split, with conversion window and step timing | 2, 3, 4, 5 |
| Retention cohorts | 4 |
| User flow (Sankey) | 3 |
| Count, unique users, sum, average | 1, 4 |
| Auto, custom and profile filters; IN, IS SET, BETWEEN | 2, 4 |
| Channel taxonomy, UTM | 2 |
| Bot tagging and the include-bots toggle | 2 |
| App versions, crashes, cold starts | 5 |
| Push notifications | 5 |
| Markdown explainer | all |

## Boards

Five boards, in list order (boards list by creation time). Each opens with the
explainer (one third) beside a hero chart (two thirds), except board 3, whose
Sankey needs the full width. The explainer rules stay as they are. Each closing
line teaches a different platform fact:

| Board | Closing line teaches |
|---|---|
| 1 | Every number is computed from raw events in ClickHouse. There is no warehouse. |
| 2 | Attribution and bot tagging happen at ingest, with no extra code. |
| 3 | Page views, clicks and rage clicks are captured automatically. |
| 4 | Anonymous visits join the profile at signup. |
| 5 | The web, iOS and Android SDKs feed the same profiles that push reaches. |

### 1. Store at a glance

"How is the shop doing this month?" Last 30 days, daily.

```
┌ explainer ─────┐┌ Revenue by category ──────────────────────┐
│                ││ stacked bars, 4 categories and Others     │
└────────────────┘└───────────────────────────────────────────┘
┌ Revenue ───┐┌ Orders ────┐┌ Avg order ─┐┌ New customers ┐
│ KPI, delta ││ KPI, delta ││ KPI, delta ││ KPI, delta    │
└────────────┘└────────────┘└────────────┘└───────────────┘
┌ Revenue by country ──────────┐┌ Orders by platform ───────┐
│ map                          ││ pie                       │
└──────────────────────────────┘└───────────────────────────┘
┌ Visitors per day ────────────┐┌ Best sellers ─────────────┐
│ area, blue                   ││ ranked list, Others row   │
└──────────────────────────────┘└───────────────────────────┘
┌ Average order by platform ────────────────────────────────┐
│ 3 lines                                                   │
└───────────────────────────────────────────────────────────┘
```

| Tile | Query | View |
|---|---|---|
| Revenue by category | Trends: sum of `amount` on `purchase`, by `category`, limit 4 and Others | Stacked bars (hero) |
| Revenue | Trends: sum of `amount` on `purchase` | KPI, compare, compact |
| Orders | Trends: count of `purchase` | KPI, compare |
| Average order value | Trends: average of `amount` on `purchase` | KPI, compare, threshold |
| New customers | Trends: count of `signup` | KPI, compare |
| Revenue by country | Map: sum of `amount` on `purchase` | Map |
| Orders by platform | Trends: count of `purchase` by `$platform` | Pie |
| Visitors per day | Trends: unique users of `page_view` | Area |
| Best sellers | Top K: `product_name` by sum of `amount`, top 8 with Others | Ranked list |
| Average order by platform | Trends: average of `amount` on `purchase` by `$platform` | Lines, full width |

### 2. Marketing & channels

"Where do customers come from, and which channels pay?" Last 90 days, daily.

```
┌ explainer ─────┐┌ Visitors by channel ──────────────────────┐
│                ││ 5 lines, growth and promo bumps           │
└────────────────┘└───────────────────────────────────────────┘
┌ Which channels convert ───────────────────────────────────┐
│ funnel split three ways: email, paid search, paid social  │
└───────────────────────────────────────────────────────────┘
┌ Where paid visitors come from┐┌ Newsletter signups ───────┐
│ map: four countries lit      ││ area, teal                │
└──────────────────────────────┘└───────────────────────────┘
┌ Paid signups ┐┌ Revenue by campaign ───────────────────────┐
│ KPI, delta   ││ ranked list                                │
└──────────────┘└────────────────────────────────────────────┘
┌ Crawler visits, by bot ───────────────────────────────────┐
│ stacked bars, include bots on                             │
└───────────────────────────────────────────────────────────┘
```

| Tile | Query | View |
|---|---|---|
| Visitors by channel | Trends: unique users of `page_view` by `$channel`, limit 4 and Others | Lines (hero) |
| Which channels convert | Funnel: `page_view`, `add_to_cart`, `purchase` within 1 day, by `$channel`, filtered with IN to Email, Paid Search and Paid Social | Funnel, three side by side |
| Where paid visitors come from | Map: unique users of `page_view` where `$channel` IN the paid channels | Map |
| Newsletter signups | Trends: count of `form_submit` where `form_name` is pupdates | Area |
| Signups from paid channels | Trends: count of `signup` where `$channel` IN the paid channels | KPI, compare |
| Revenue by campaign | Top K: `$utmCampaign` by sum of `amount`, where `$utmCampaign` is set | Ranked list |
| Crawler visits, by bot | Trends: count of `page_view` by `$bot_reason`, include bots on, `$bot` is true, limit 4 and Others | Stacked bars, full width |

Top referrers, pages, devices and countries are already on the Overview page, so
this board leaves them out.

### 3. Shopper journeys

"How do people move through the store, and where do they drop off?" Last 30
days, daily. The Sankey leads at full width, and the explainer sits in row 2.

```
┌ How shoppers move ────────────────────────────────────────┐
│ Sankey over page types, 5 columns                         │
└───────────────────────────────────────────────────────────┘
┌ explainer ─────┐┌ Rage clicks ──────────────────────────────┐
│                ││ area, red                                 │
└────────────────┘└───────────────────────────────────────────┘
┌ Search to order ─────────────────────────┐┌ Avg basket ────┐
│ funnel: blue, violet, emerald, green     ││ KPI, delta     │
└──────────────────────────────────────────┘└────────────────┘
┌ Searches per day ────────────┐┌ What dogs search for ─────┐
│ column bars, violet          ││ ranked list               │
└──────────────────────────────┘└───────────────────────────┘
┌ How far shoppers scroll ─────┐┌ How they pay ─────────────┐
│ ranked list, average %       ││ pie                       │
└──────────────────────────────┘└───────────────────────────┘
```

| Tile | Query | View |
|---|---|---|
| How shoppers move | User flow: `page_type` on `page_view`, 4 hops, 10 nodes per step | Sankey (hero), full width |
| Rage clicks | Trends: count of `rage_click` | Area |
| Search to order | Funnel: `page_view`, `search`, `add_to_cart`, `purchase` within 1 day, step timing | Funnel |
| Searches per day | Trends: count of `search` | Column bars |
| What dogs search for | Top K: `query` on `search` | Ranked list |
| How far shoppers scroll | Top K: `page_type` by average of `percent` on web `scroll` | Ranked list |
| How they pay | Trends: count of `purchase` by `payment_method` (a new property) | Pie |
| Average basket | Trends: average of `item_count` on `checkout_started` | KPI, compare |

### 4. Customers & loyalty

"Who comes back, and who pays?" Last 180 days, weekly.

```
┌ explainer ─────┐┌ Weekly shoppers by platform ──────────────┐
│                ││ stacked bars                              │
└────────────────┘└───────────────────────────────────────────┘
┌ Do new customers come back? ──────────────────────────────┐
│ cohort table                                              │
└───────────────────────────────────────────────────────────┘
┌ Orders: members vs everyone else ─────────────────────────┐
│ grouped bars, two a week                                  │
└───────────────────────────────────────────────────────────┘
┌ From Club page to paid plan ─────────────┐┌ New Club ──────┐
│ funnel: blue, yellow, gold, green        ││ column bars    │
└──────────────────────────────────────────┘└────────────────┘
┌ Club revenue by plan ─────────────────────────────────────┐
│ 3 lines                                                   │
└───────────────────────────────────────────────────────────┘
┌ Top customers ───────────────┐┌ Satisfaction ─────────────┐
│ ranked list, dog profiles    │└───────────────────────────┘
│                              │┌ What senior dogs buy ─────┐
└──────────────────────────────┘└───────────────────────────┘
```

| Tile | Query | View |
|---|---|---|
| Weekly shoppers by platform | Trends: unique users of `product_viewed` by `$platform` | Stacked bars (hero) |
| Do new customers come back? | Retention: `signup`, then `product_viewed`, weekly | Cohort table, full width |
| Orders: members vs everyone else | Trends: count of `purchase` by `membership` (a new property) | Grouped bars, full width |
| From Club page to paid plan | Funnel: `page_view` where `page_type` is Club, `trial_started`, `subscription_started`, `invoice_paid` within 14 days, step timing | Funnel |
| New Club members | Trends: count of `subscription_started` | Column bars |
| Club revenue by plan | Trends: sum of `amount` on `invoice_paid` by `plan_name` (a new property there) | Lines, full width |
| Top customers | Top K: user by count of `purchase` | Ranked list with profile |
| Customer satisfaction | Trends: average of `score` on `nps_submitted` | KPI, threshold (a 180-day board has no prior window to compare) |
| What senior dogs buy | Top K: `category` by sum of `amount`, profile `age_years` between 8 and 16 | Ranked list |

### 5. Mobile app & push

"How are the apps doing, and does push pay?" Last 30 days, daily.

```
┌ explainer ─────┐┌ App version adoption ─────────────────────┐
│                ││ stacked bars, versions rolling over       │
└────────────────┘└───────────────────────────────────────────┘
┌ Crashes per day ─────────────┐┌ Cold start ───────────────┐
│ column bars, the bad release ││ KPI, seconds              │
│                              │┌ iOS vs Android ───────────┐
└──────────────────────────────┘└───────────────────────────┘
┌ Push to purchase ────────────┐┌ Push opens ───────────────┐
│ funnel                       ││ area, pink                │
└──────────────────────────────┘└───────────────────────────┘
┌ Where the app is used ───────┐┌ Top errors ───────────────┐
│ map                          ││ ranked list               │
└──────────────────────────────┘└───────────────────────────┘
```

| Tile | Query | View |
|---|---|---|
| App version adoption | Trends: unique users of `app_open` by `$app_version`, limit 4 and Others | Stacked bars (hero) |
| Crashes per day | Trends: count of `app_crashed` | Column bars |
| App cold start | Trends: average of `cold_start_ms` on `app_open` (a new property) | KPI, compare, duration |
| iOS vs Android | Trends: unique users of `app_open` by `$os` | Pie |
| Push to purchase | Funnel: `notification_received`, `notification_clicked`, `app_open`, `purchase` within 1 hour, step timing | Funnel |
| Push opens | Trends: count of `notification_clicked` | Area |
| Where the app is used | Map: unique users of `app_open` | Map |
| Top errors | Top K: `error_code` on `error_occurred`, every platform | Ranked list |

### Sankey

Checked on 2026-11-06 by rendering variants through the real FE chart, on today's
data and on simulated target flows. A Sankey over `$pathname` is unreadable: every
session starts at `/`, the 46 product URLs collapse into "Others", and labels are
cut off. So the rules are:

1. **Nodes come from a small vocabulary**, never URLs or ids. Every web event
   carries a custom `page_type`.
2. **Every label gets its own colour.** The FE hashes a node's label into its
   palette, and labels that start like an event family take that family's hue
   ("Order confirmed" turns commerce green through `order_`). The labels below were
   checked against the FE's colour function and land on ten different colours.
3. **Sessions start in different places.** The landing page depends on the channel,
   so the first column has five or six nodes, not one giant Home.
4. **Sessions walk a real path.** A web session views 3 to 7 pages along a
   transition table (Home to Collections, Search or Product; Product to Cart; Cart to
   Checkout; Checkout to Confirmation), and can leave at every step. Transitions
   under 1% of sessions are left out, so no column ends in a pile of slivers.
5. **Four hops, full width, about 32 rows tall.** Each column needs about 170px, so a
   five-column Sankey at two-thirds width scrolls sideways. `max_nodes` is 10, at
   least the vocabulary, so no "Others" node appears.

| Labels (`page_type`) | Landing mix |
|---|---|
| Home, Collections, Product, Search, Cart, Checkout, Confirmation, Guide, Club, Account | Home 38%, Product 26% (ads, social), Collections 20% (email), Guide 10% (organic search), Search 5% (direct) |

App screens rendered just as well over Home, Collections, Product, Search, Cart,
Checkout, Confirmation, Account. One Sankey is enough for now, so the app board
does without.

### Funnels

A funnel with no breakdown colours each step by its event's family: navigation is
blue, search violet, commerce green, push pink, app indigo, billing gold, auth
slate. A split funnel draws one colour per series from the breakdown palette.
Rendered on 2026-11-06: page view, search, add to cart, purchase draws blue,
violet, emerald and green. The all-commerce checkout funnel draws four greens. So:

1. **An unsplit funnel crosses at least three families.** Never seed an
   all-commerce funnel without a split.
2. **A split funnel has three series, side by side.** The palette's first three
   colours are vivid; the fourth and fifth are grey and black. The FE lays split
   funnels out two to a row today, so three leave a hole (see "Frontend asks").
3. **The series must differ.** Today every channel converts at about 18%, which
   makes the split say nothing. The generator gives each channel and platform its
   own rate.
4. **No funnel ends near zero.** Today a video funnel ends at 0% and the club funnel
   at 1%.
5. **Room for the series list.** A split funnel needs about 40 rows, or its list is
   cut off.

| Funnel | Steps | Colours |
|---|---|---|
| Which channels convert (2) | split by `$channel`, three channels | indigo, yellow, fuchsia |
| Search to order (3) | `page_view`, `search`, `add_to_cart`, `purchase` | blue, violet, emerald, green |
| From Club page to paid plan (4) | Club `page_view`, `trial_started`, `subscription_started`, `invoice_paid` | blue, yellow, gold, green |
| Push to purchase (5) | `notification_received`, `notification_clicked`, `app_open`, `purchase` | pink, pink, indigo, green |

Three of the four colour each step. The split one gives each channel its own
colour, because comparing channels is its point.

## Data

### The story

Six months of history, then live. A visitor should be able to read these:

| Beat | When | Where it shows |
|---|---|---|
| Steady growth, about 6% a month | the whole window | every trend, and positive KPI deltas |
| A gentle weekly rhythm: weekends about 8% above weekdays | every week | daily charts |
| A promo every six weeks, about 1.6x revenue | recurring | Revenue by category, Visitors by channel, Revenue by campaign, Newsletter signups |
| An app release every three weeks. One ships a crash and slower cold starts, and a hotfix follows two days later. | recurring | App version adoption, Crashes per day, App cold start |
| A push blast day each cycle | recurring | Push opens, Push to purchase |
| Email converts best, paid social worst | always | Which channels convert |
| Apps convert better than web | always | Average order by platform |
| Senior dogs buy beds and food | always | What senior dogs buy |
| Satisfaction is high and inching up | always | Customer satisfaction |
| Paid campaigns run in four countries; organic traffic is global | always | Where paid visitors come from |
| The apps are biggest in India, Brazil and Indonesia | always | Where the app is used |
| Club members order about twice as often | always | Orders: members vs everyone else |

### Generator changes

| Change | Fixes or feeds | Priority |
|---|---|---|
| Time-driven backfill. Walk every minute from the start to now at the live rate (`PUG_DEMO_PEAK_SESSIONS_PER_MIN` times `TrafficFactor`), with the same session builder as live. `PUG_DEMO_SEED_COUNT` goes away. | The density cliff and the climbing retention | must |
| Six months of history. The longest board window is 180 days. (Done.) | Board 4 | must |
| A growth factor in `TrafficFactor`, capped so live traffic levels off | KPI deltas, every trend | must |
| Tamer promos: traffic 1.25x and a smaller purchase boost | No 5x towers | must |
| Flatten `dayWeights` to about 8% either side of 1. Scale the desktop, mobile and weekend hour tables to the same daily total, so they set when people shop, not how much. | Weekend spikes | must |
| 50,000 users instead of 10,000, each visiting less often | Believable top customers and retention | must |
| A real channel mix: organic search, direct, paid search, paid social, email, organic social, referral (pet blogs), video | Visitors by channel | must |
| A conversion rate per channel and per platform: email best, paid social worst, apps above web | Which channels convert | must |
| A `page_type` on every web event, from the Sankey vocabulary. One `page_view` per page a session visits, walking the transition table from a landing page set by the channel. (Done but the transition table.) | How shoppers move, How far shoppers scroll, top pages | must |
| Tag bot sessions with `$bot` and `$bot_reason`, using the same `botdetect` matchers as ingest, the way `applyAttribution` mirrors attribution. Add AI crawlers. (Done.) | Crawlers counted as people; Crawler visits | must |
| Every web event keeps the landing referrer, as a single-page store's `document.referrer` does, so `$channel` holds for the whole session (done) | Visitors by channel | must |
| A release every three weeks, fast adoption, one crashing release with a hotfix | Board 5 | must |
| `cold_start_ms` on `app_open`, slower on the crashing release (done, slower after every release until that release exists) | App cold start | must |
| Errors on iOS and Android too (done) | Top errors | must |
| Push blasts with a 4 to 10% click rate. Clicks lead to purchases within an hour. | Push to purchase | must |
| Club trials convert about 40%. Done for live traffic: a session 3 to 14 days after a trial converts it once, at 45%, on the trial's plan, and a subscriber never trials again. The count-driven backfill converts about 25% until the time-driven backfill lands. Some Club page views don't start a trial (done). Members renew each month (`invoice_paid`, with `plan_name`; `plan_name` done). | From Club page to paid plan, Club revenue by plan | must |
| `payment_method` on `purchase` (done) | How they pay | must |
| Scroll depth by page type: guides read deepest, carts barely scroll (done) | How far shoppers scroll | must |
| `product_name` on `purchase` (done) | Best sellers | must |
| NPS scores skew high (mostly 8 to 10, done) and drift up | Customer satisfaction | must |
| Paid campaigns target only the US, UK, Canada and Australia | Where paid visitors come from | must |
| App users lean to India, Brazil and Indonesia, mostly on Android. Today every map has the same shape. | Where the app is used | must |
| `membership` on `purchase` (Member or Non-member, matching the profile's `pug_club`, done), and members order about twice as often | Orders: members vs everyone else | must |
| Category preference by dog age | What senior dogs buy | should |
| App sessions walk a screen table over the app vocabulary | A later app Sankey | could |
| A share of EU web visitors without cookies (`cookieless-` ids) | The include-cookieless toggle | could |

Two smaller fixes ride along:

1. `pug seed --no-reset` clears only `events` and `profiles`. It should clear
   the same tables as project purge, or rollups and activity states survive.
2. The old seed comment says the FE renders neither Sankey nor map tiles. It
   renders both now. (Done: the comment is gone.)

## Rebuilding the demo in production

### Today

The worker backfills once, when the demo project has no events. The demo project
is the demo org's oldest live project, for both the seeder and `DemoSignIn`.
Purge refuses a live project, so the only way to replace its data is to delete
it: the worker then gives the demo org a new project and backfills it.

### How the rebuild works

Not built: the manual rebuild in "The plan" replaced it. The side effects at the
end of this section apply to the manual rebuild too.

The worker carries a date constant, `demoDataEpoch`. A demo project created before
it is stale. There is no new table: the project's own `create_time` is the marker.

On start, the worker:

1. Finds the demo org's live projects. The oldest is the current demo.
2. Starts live traffic if the current demo was created on or after
   `demoDataEpoch`. That is the normal path.
3. Otherwise deletes every other live project in the demo org. Those are left over
   from an interrupted build.
4. Creates a new project in the demo org and builds it: dashboards, backfill,
   profiles. Visitors keep seeing the old demo meanwhile.
5. Deletes the old project, the same delete an admin runs. That one delete is the
   switch: the new project is now the oldest live one, so `DemoSignIn` returns it.
6. Starts live traffic on the new project. The purge cron erases the old one
   after deletion's usual one-hour wait.

It cannot loop. The new project is created after the epoch, so the next start
finds it current. A test fails the build if `demoDataEpoch` is later than today,
so it is always the release day or earlier.

If the worker dies during step 4, the old project is still the oldest and still
stale. The next start deletes the half-built project in step 3 and builds again.
If it dies after step 5, the new project is the oldest and current, so the next
start goes live.

Guardrails:

1. It touches only the demo org, the one org both demo accounts belong to, and
   only while `PUG_DEMO_ENABLED` is on.
2. The build runs under an advisory lock. The Deployment already runs one
   replica with `maxSurge: 0`.
3. The backfill is paced, so production ClickHouse stays responsive for real
   tenants.

Why not wipe in place: purge refuses live projects on purpose; the demo would be
empty for minutes; and the wipe would run while live traffic still writes.
Building beside the old one reuses the delete path production already runs.

Side effects:

1. The project id changes. Nothing links to it; the site links only to
   demo.pug.sh. Old crawled URLs stop resolving.
2. Open demo tabs fail until they reload.
3. The demo org's usage counts both projects for the current period, because
   deletion keeps usage. The demo org is not billed.
4. The old project is the largest tenant, about 5M events. Purging it rewrites
   every ClickHouse part that holds its rows, so expect some minutes of merge
   load. Release at a quiet hour.
5. Boards an operator added to the old demo project by hand go with it.

## Tests

1. **Every tile renders.** A new integration test seeds a short window, runs
   each seeded tile through the real insights executor, and checks it returns
   data: no empty tile, at most 5 series, a funnel's last step above zero, a
   split funnel whose series differ, a Sankey with five columns and no "Others"
   node, a map with countries. Today a tile can pass every Go test and still
   render blank.
2. The existing layout, validation and encoding tests stay.
3. Generator tests pin the new rates: channel conversion order, push click rate,
   club conversion, the crash spike on the bad release, the weekly rhythm, and
   bot tags that match `botdetect`.
4. Rebuild test: a deleted demo project gets a new one in the demo org, which
   can seed its profiles before the purge runs, and later seeds keep it (done).

## Frontend asks

In the app repo. The first one is needed for the three-way funnels; the rest are
optional.

1. Lay a split funnel's series out three to a row on a full-width tile. Today the
   grid is always two wide, so a three-way split leaves a hole. Without this, the
   split funnels split two ways.
2. Six saturated breakdown colours by default. Today's palette is indigo, yellow,
   fuchsia, then grey and black, so a hero split five ways ends in grey and
   black. Pug's colourblind mode already has six saturated hues per polarity.
3. Draw the dashed prior period on single-series line and area tiles that set
   `compare`. The overview chart already does; dashboard tiles use `compare` only
   for the KPI delta.
4. A currency format for money axes and KPIs. The proto has none; compact
   numbers are the stopgap.
5. Round the legend's average for unique-user series. `401.565` reads like a
   bug.
6. Draw the current, unfinished bucket faded or dashed. Weekly charts end in a
   dip.
7. A broken-down funnel says "Showing top 0" under its series list.
8. Give a breakdown value the same colour in every tile. Colours go by rank
   today, so one app version can be indigo in one chart and yellow in the next.
9. Draw a trends split's `$others` series in the muted ink, as the top-K list
   draws its `$others` row. It took the palette's fifth colour, near-black on
   light and near-white on dark. (Done in the app, not merged yet.)

## Open questions

1. The automatic rebuild, or a manual delete? The manual delete, repeated at each
   refresh (2026-11-06).
2. 50,000 users and about 35k events a day: is that the size of shop to show?
3. Board 3 leads with the Sankey and puts the explainer in row 2. Is that
   exception fine?
4. Do the palette change and the cookieless share belong in this round?
