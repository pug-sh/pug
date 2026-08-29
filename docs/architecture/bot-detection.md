# Bot detection

> **Status: PLUMBING BUILT, TOGGLE PENDING.** Written 2026-08-29; the storage
> and tagging half was built the same day — migration 012, `internal/botdetect`,
> `enrichBot`. Still to do, in order: the Cloudflare Transform Rule that turns
> signal 2 on, the `include_bots` toggle, the SDK check. One thing changed in
> implementation and is marked **Amended** below: the signals are scoped to
> browser traffic.

## The problem

Some "visitors" are programs: uptime monitors loading a page every minute,
scrapers driving a real Chrome with no screen, SEO tools, Lighthouse runs. They
execute our JavaScript, so they send events exactly like a person, and pug has
no way to tell them apart. They land in every dashboard, mostly as Direct
traffic with one-page sessions.

Classic crawlers (Googlebot, GPTBot) are *not* this problem for the browser SDK:
they mostly don't run JavaScript, so they rarely reach it (Googlebot's rendering
pass is on the list anyway). They only matter for a server-side ingest path.

## Today

The ingest handler already reads `CF-Bot-Score` and `CF-Verified-Bot` into the
`bot_score` and `verified_bot` columns. Both need Cloudflare Bot Management, an
Enterprise add-on we will not buy, so both columns are empty and nothing filters
on them. They stay as they are — their meaning ("Cloudflare's score", "a
known-good bot") is not what this design records.

## Design: three signals, one tag, one toggle

Two server-side signals (the user agent, the origin network) and one in the
browser. The server *tags* — never drops — what it finds, so a project owner
can include or exclude the traffic per query.

### Signal 1 — user-agent list (server)

`github.com/monperrus/crawler-user-agents` (MIT, 1,500 patterns, updated
monthly) as a normal Go dependency. Its Go package pre-parses every pattern at
init — nearly all collapse to plain-string matches in one `strings.Replacer`,
the rest to Go's backtracking-free `regexp` behind a literal pre-filter — so
matching is bounded on a user agent an attacker controls; `botdetect` also caps
the scanned length at 2 KB, since those few regexps rescan per literal hit.
It knows HeadlessChrome, Playwright, Puppeteer, Selenium, Datadog Synthetics,
Checkly and Lighthouse — the things that actually run our JavaScript. Checked once per request (a batch shares one `User-Agent`), not once
per event. The reason recorded is one stable name per list entry
(`HeadlessChrome`, `Googlebot`, `DatadogSynthetics`), derived at startup from the
entry's own example user agent — never the pattern itself (the list stores regex
source, `Googlebot\/`, `S[eE][mM]rushBot`) and never text from the live request,
which would be attacker-chosen.

### Signal 2 — datacenter network (server, via Cloudflare)

Cloudflare knows which network (ASN) every request comes from. A Transform Rule
on the zone adds it as a request header — `CF-ASN` set to
`to_string(ip.src.asnum)` — free on every plan, with no IP-range list to
download or keep fresh. The handler compares the number against a short Go list,
`datacenterASNs`, of networks that only host servers. Same trust model as
`CF-IPCountry`: it means something because the origin is only reachable through
Cloudflare; a self-hosted pug without the rule just has this signal off.

Membership rule: a network goes on the list only if essentially nobody browses
from it. Starter list:

| ASN | network | note |
|---|---|---|
| 16509, 14618 | Amazon (AWS) | AWS has dozens of ASNs; these two carry the bulk. Tail: AWS WorkSpaces desktops. |
| 396982 | Google Cloud | Cloud VMs only. |
| 8075 | Microsoft (Azure) | Also Windows 365 / AVD desktops and corporate egress — a human tail we accept. |
| 24940 | Hetzner | |
| 14061 | DigitalOcean | |
| 16276 | OVH | |
| 63949 | Linode (Akamai cloud) | Not Akamai's CDN ASNs — see below. |
| 20473 | Vultr | |
| 31898 | Oracle Cloud | |
| 12876 | Scaleway | |
| 51167 | Contabo | |
| 8560 | IONOS | |
| 40509 | Fly.io | |
| 45102 | Alibaba Cloud | |
| 132203 | Tencent Cloud | |

Kept off on purpose: **15169 Google** (Googlebot, but also Chrome's IP
Protection proxy and Google Fi's VPN — real people), **13335 Cloudflare, 54113
Fastly, 20940 Akamai** (WARP, iCloud Private Relay and Edge Secure Network exit
there — real people), and every commercial VPN network. The list is "most of the
cloud", not all of it, and that is fine.

### Signal 3 — automation flag (browser SDK)

In `@pug-sh/browser` (the `sdk-web` repo): if `navigator.webdriver` is true, or
the user agent / `navigator.userAgentData.brands` contains `HeadlessChrome`,
don't send at all. It costs nothing, stops Playwright/Puppeteer/Selenium before
they use any ingest or metered quota, and a bot that hides it is caught by
signal 1 or 2 if it can be caught at all. Not sending (rather than tagging) is
deliberate: a client-sent "I am a bot" flag can't be trusted anyway, and nobody
has asked to analyse their own automation traffic.

### Amended: which traffic the signals apply to

Both server signals run only on **public-key requests**, and within those tag
only events with **`$platform = web`**. The crawler list also names HTTP-client
libraries — `okhttp` and `Go-http-client` are exactly what the React Native
(Android) and Go SDKs send, neither setting a User-Agent of its own; the Node
SDK sends none at all — and datacenter networks are where every customer
backend lives. A private-key request is the customer's own server and a non-web
event is a native or server SDK; neither is a visitor, so neither is tagged. A
bot that runs the web SDK cannot avoid `$platform = web` — the SDK re-asserts it.

### What gets stored

Two promoted columns on `events` (migration 012), written by `enrichBot` right
after `enrichUserAgent` in the chain: `bot Bool DEFAULT false` and
`bot_reason LowCardinality(String) DEFAULT ''` — the crawler's name
(`HeadlessChrome`, `Googlebot`, …) or `asn:24940`; the user-agent wins
when both signals fire. Both are server-only auto-properties (`$bot`,
`$bot_reason`): a client-sent value is stripped before enrichment, like
`$referrerDomain`. `$bot_reason` is a promoted string column, so the filter
picker lists it and a dashboard can already break down by it. The counter
`events.bot_tagged_total{project_id, signal}` counts tagged events by signal
(`user_agent` / `asn`); an
unparseable `CF-ASN` counts on `events.cdn_header_parse_failed_total`. Nothing
is dropped.

### Query toggle

`include_bots` on `InsightQuerySpec` (field 14), default false = bots excluded.
It mirrors `include_cookieless` but is wider: it applies to every metric and
every insight, because a monitor's pageviews are wrong in a pageview count just
as much as in a visitor count. The raw path is `WHERE bot = 0`. The rollup fast
path needs `bot` as a key column on `dashboard_event_rollup_daily` and the
session rollup — migration 012 added it exactly the way 011 added `cookieless`
(new UInt8 in the ORDER BY, no DEFAULT, MODIFY QUERY on the MV, computed as
`toUInt8(bot)`). Rows from before the migration read `bot = 0`, so older history
counts as human — the same kind of split as the channel history, accepted the
same way. Until the toggle exists nothing reads the key column; unpredicated
reads merge both values (`TestIntegrationBotTagging`). The session rollup's key
is per event, so a session whose events straddle `bot = 0/1` — `$platform` is
client-supplied, sessions are in flight at the deploy, the ASN is per request —
sits in two rows. The toggle's session predicate must therefore be
session-level, `HAVING max(bot) = 0` after the merge: a row-level
`WHERE bot = 0` would return half a session, with a false bounce and truncated
entry/exit values.

## What it misses, and what it gets wrong

**Misses:** automation using a normal Chrome user agent from a residential or
mobile IP — bot networks doing exactly this exist. No free signal catches it.

**False positives:** people on a VPN or Tailscale exit node hosted in
Hetzner/DigitalOcean/AWS, corporate proxies on Azure/AWS, Windows 365 or AWS
WorkSpaces desktops — and, tenant-wide rather than a tail, a customer that
proxies the collector through its own backend to dodge blockers: Cloudflare
sees that backend's network as `ip.src`, so every event it forwards is tagged.
This is why the tag is not a drop — a project owner can flip `include_bots` and
see the traffic. If it ever matters, the fix is a per-project "never tag" list,
not a smaller global one.

## Rollout order (the app is live)

1. ✅ Migration 012: `bot` and `bot_reason` on `events`, `bot` key column on
   both rollups. Lands via PreSync before anything writes them — bump the
   migrate image digest in the same gitops commit as the server and worker
   images, or the new INSERT fails `NO_SUCH_COLUMN` and every batch goes to
   the DLQ while SDKs still see 200 (see web-analytics.md's deploy runbook).
2. ✅ Server: the dependency, `datacenterASNs`, `enrichBot`, the two
   auto-properties, the counter. Tagging starts; no query changes yet.
3. Cloudflare: the Transform Rule. Signal 2 goes live by itself.
4. API + FE: `include_bots` and its predicate on both paths. This is the one
   step where dashboards change — worth a release note.
5. SDK: the webdriver check, on the SDK's own release cadence.

## Later, separately: hostname allowlist

Public keys are extractable, so anyone can paste a project's snippet onto
another site (or a staging domain) and pollute its data. The answer is a
per-project list of allowed hostnames: a project setting checked against
`$hostname` at ingest, dropping (there is nothing to analyse) and counting the
rejects. Its own change: Postgres column, RPC, cache invalidation.

## Open questions

- 8075 (Microsoft) and 16509 (Amazon) in or out? **In** (shipped) — that is
  where the automation runs — accepting the desktop-in-the-cloud tail.
- Revive `verified_bot` via the free `cf.client.bot` field? Not for the browser
  SDK: verified bots mostly don't run JavaScript.
