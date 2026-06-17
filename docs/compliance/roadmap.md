# DPDP / GDPR Compliance Roadmap

Status: **planning.** First item shipped — §4.4 (stop storing raw IP) is implemented in [PR #15](https://github.com/pug-sh/pug/pull/15) (pending merge); everything else below is still planning. This is the working reference for making Pug Cloud customers DPDP- and GDPR-compliant. It supersedes the three bullets under "Data Governance" in [`../TODO.md`](../TODO.md).

Scope decision: build for the **stricter superset of both regimes** (India's Digital Personal Data Protection Act 2023 + EU GDPR), so a single coherent compliance story satisfies both. Where the two diverge, the tighter requirement wins.

Legend: ❌ missing · ⚠️ partial · ✅ present.

---

## 1. Roles — and what "make my customers compliant" actually means

| Role | GDPR term | DPDP term | Who, here |
| --- | --- | --- | --- |
| Pug Cloud | **Processor** | **Data Processor** | Processes data on customers' documented instructions |
| Our customer | **Controller** | **Data Fiduciary** | Decides why/how data is processed; carries the primary legal duty |
| The customer's website visitor | **Data Subject** | **Data Principal** | The person being tracked |

We cannot make a customer compliant on paper — that is their controller/fiduciary duty. What we **can** and **must** do is two things:

1. **Build the mechanisms** a controller needs to discharge their duties: erase-this-person, export-this-person, configurable retention, consent controls. If these don't exist, the customer *cannot* be compliant no matter what they sign, and we are the blocker.
2. **Meet our own processor duties**: reasonable security safeguards, sub-processor transparency, breach notification, and a Data Processing Agreement (DPA) they can sign.

Everything below serves one of those two goals.

---

## 2. Two data-subject populations (don't conflate them)

- **Dashboard users** — our customers' own staff. Stored in PostgreSQL `customers`, `customer_identities`, `org_members`. Email, name, OAuth subject, password hash. Standard SaaS-account data; lower risk.
- **End-users** — our customers' website visitors. This is the analytics/event/profile data and is where **every hard problem lives**. The rest of this doc is about end-users unless stated otherwise.

---

## 3. Personal-data inventory (where end-user data physically lives)

| Data | Collected | Stored | Notes |
| --- | --- | --- | --- |
| `anonymousId`, `externalId`, `sessionId` | SDK | ClickHouse `events.distinct_id` / `session_id`; PG + CH `profiles.external_id` | `distinct_id` is *both* anon and external IDs over a lifetime — see erasure |
| **Raw IP address** | server-side from `CF-Connecting-IP` (`internal/geo/cloudflare.go`), transient — geo enrichment only | **Not stored** ✅ (was `events.auto_properties['$ip']`; removed in [PR #15](https://github.com/pug-sh/pug/pull/15)) | Personal data under both regimes; now stripped before storage — see §4.4 |
| Geo (country/region/city/lat-lon/postal) | derived from IP | ClickHouse promoted columns + `auto_properties` | Derived from the IP above |
| User-agent / device / OS / browser | SDK Client Hints or server UA parse | ClickHouse promoted columns | Fingerprinting surface when joined to an identifier |
| Page URL, referrer, title, UTM | SDK | ClickHouse `url` + `auto_properties` | URLs/titles can embed PII (tokens, emails, names) |
| Click text (≤50 chars), form id/name/action, scroll, rage/dead-click | SDK autocapture | ClickHouse `custom_properties` | Click `text` can capture sensitive labels |
| Profile traits | `identify(externalId, traits)` | PG `profiles.properties` (jsonb), CH `profiles.properties` | Customer-controlled; can be anything, incl. email/name |
| Push device token + endpoint | `subscribePush()` | PG `profile_devices.token` | Delivery secret |
| Custom event properties | `track(kind, props)` | ClickHouse `custom_properties` | Customer-controlled; stored as-is, unfiltered |

Retention today: NATS streams expire at 720h (30d); **ClickHouse and PostgreSQL never expire anything.**

---

## 4. Gap analysis & roadmap

Each item: what the code does today (with file references), what to add, and the acceptance bar.

### P0 — Data-subject rights (the legal core)

#### 4.1 Erasure that reaches the events ❌ — the headline failure

**Today:** `ProfilesService.Delete` → `internal/core/profiles/service.go:107` soft-deletes the profile (PG `deletion_time = now()`; CH upsert with `is_deleted = 1`) and deactivates devices. **Nothing touches the `events` table.** The person's entire behavioral history stays forever. `schema/clickhouse/migrations/001_create_events_table.sql` has no `is_deleted`, no TTL, and `distinct_id` isn't even in the sort key. This fails GDPR Art. 17 and DPDP §12.

**The hard part — identity fan-out:** a person's events are keyed by `distinct_id`, which over their lifetime is *both* the `anon-<uuid>` value(s) *and* the `external_id`. The alias history is in `profile_aliases`. A complete erasure must resolve **all** distinct_ids for the profile first.

**Add:**
1. A `DeleteDataSubject(external_id)` RPC on the shared/SDK surface (so a controller can wire a "delete me" button), not just the internal profile-ID path. Proto: `proto/shared/profiles/v1/profiles.proto`.
2. A dedicated **erasure worker** (`internal/app/workers/compliance/` — a generalized compliance worker that also hosts §4.2 export and §4.5 retention) — ClickHouse `ALTER … DELETE` mutations are heavy and async; never run them inline in the RPC. Steps:
   - resolve all distinct_ids from `profile_aliases`,
   - `ALTER TABLE events DELETE WHERE project_id = ? AND distinct_id IN (…)`,
   - delete rows in `profiles`, `profile_aliases`, `profile_activity_summary` (PG + CH),
   - delete `profile_devices` (push tokens).
3. A **deletion-request table** (PG) recording request → status → completion timestamp, so we can *prove* fulfilment within the statutory window (GDPR ~1 month; DPDP timelines). This is also the DSAR audit trail.

**Done when:** after a delete request completes, the subject's profile *and* every event row across all their distinct_ids are gone from ClickHouse, the request row shows `completed`, and a re-query returns nothing.

#### 4.2 Export / access ❌ — scoped in [`4.2-export-scope.md`](./4.2-export-scope.md)

**Today:** no endpoint aggregates a subject's data. `ProfilesService.Get/GetByExternalId/List` return profile rows only, never events.

**Add:** `Export(id)` / `ExportDataSubject(external_id)` — a **shared/private** (never SDK) RPC that **server-streams** profile traits + full event history (paginated), reusing 4.1's alias fan-out (`events.Reader.GetActivityFeed`/`resolveProfileIDs`) and writing a `compliance_requests` audit row (kind `export`). The proto stream is the machine-readable artifact (JSON via Connect's codec); CSV is a client-side concern. Satisfies GDPR Art. 15 (access) and Art. 20 (portability), DPDP §11. No worker and no new infra — a durable **object-storage artifact + signed URL** is the documented future scale path (slots into the compliance worker; see the scope doc §7).

**Done when:** one call returns everything we hold about a subject, paginated for large histories, with a `completed` audit row proving fulfilment.

#### 4.3 Rectification ⚠️

**Today:** `identify()` shallow-merges traits but cannot delete one. **Add:** ability to null/remove individual profile properties.

### P1 — Storage limitation & IP minimisation

#### 4.4 Stop storing raw IP ✅ — done in [PR #15](https://github.com/pug-sh/pug/pull/15) (pending merge)

**Was:** the Cloudflare provider wrote the full client IP into `auto_properties['$ip']` (`internal/geo/cloudflare.go`), where it was persisted forever. Geo was derived from it and the raw value kept for no clear reason.

**Done:** the IP is now used only transiently for geo enrichment and never stored. GDPR Recital 30; DPDP treats IP as personal data.

- `CloudflareProvider.Locate` no longer emits `$ip` into the `Location`; `geo.ClientIP` remains as the transient extraction primitive for IP-lookup providers (e.g. MaxMind), which must likewise keep it out of the returned `Location` (`internal/geo/cloudflare.go`, `internal/geo/geo.go`).
- `enrichGeo` **strips any client-supplied `$ip`** from every event (and defensively drops `$ip` from the provider `Location`) — beyond the original plan, this closes the hole where an untrusted SDK caller could smuggle an IP into NATS/ClickHouse via `auto_properties` (`internal/app/server/rpc/sdk/events/handler.go`).
- Dropped the `$ip` read-back: removed `ProfileStats.IP`, its ClickHouse `argMax(auto_properties)` column, and the `ip` field (#11) on the `activity.ProfileStats` proto (`internal/core/events/reader.go`, `internal/app/server/rpc/shared/activity/handler.go`, `proto/shared/activity/v1/activity.proto`).
- Tests assert `$ip` is stripped from client input (even when geo is empty) and never reaches storage; `docs/architecture/ingestion.md` and `profiles.md` updated in the same PR.

**Done when:** ✅ no IP reaches ClickHouse at all; geo columns still populate.

#### 4.5 Configurable retention + TTL ❌

**Today:** nothing in ClickHouse or PostgreSQL ever expires. DPDP §8(7) *actively requires* erasure once the purpose is served; GDPR Art. 5(1)(e) requires storage limitation.

**Add:** a per-project retention setting, enforced by either ClickHouse `TTL occur_time + INTERVAL N MONTH` or a scheduled partition-drop/mutation job filtered by `project_id` (TTL is per-table but retention is per-project, so a job is likely needed). Plus a purge job for soft-deleted profiles past a grace window. Doubles as a sellable feature ("configurable data retention").

**Done when:** events older than a project's retention window are provably gone, on a schedule.

### P1 — Consent & capture hygiene

#### 4.6 Capture masking in the SDK ⚠️ (`../../../sdk-web`)

**Today:** autocaptured click `text` (≤50 chars), full `$url`, and form metadata routinely carry PII.

**Add (SDK):** `data-pug-mask` / `data-pug-no-capture` attributes, default input masking, a URL query allowlist/stripper, and a `maskAllText` option (PostHog-style). The proto already defines an **unused** `(pii) = true` annotation in `proto/common/events/v1/options.proto` that could drive server-side redaction.

#### 4.7 Consent: signals, default, accountability ⚠️ (`../../../sdk-web`)

**Today:** the SDK's opt-in/opt-out (`src/tracking-consent.ts`) is genuinely good, but (a) it doesn't honor **DNT / Global Privacy Control**, (b) default consent is `'granted'`, and (c) consent isn't recorded server-side so the controller can't *demonstrate* it.

**Add:** an opt-in `respectDNT`/GPC mode; a documented region-aware **deny-by-default** mode (EU/ePrivacy expects opt-in *before* any localStorage write); and either server-side recording of the consent basis on events or clear documentation that gating is client-side. Note the SDK writes ~4 localStorage keys (session, profile, consent, device) → ePrivacy "terminal equipment" consent territory.

#### 4.10 Cookieless server-hash identity ❌ — scoped in [`4.10-cookieless-identity.md`](./4.10-cookieless-identity.md)

**Today:** anonymous identity is always a client-stored UUID (`anon-<uuid>` in `sdk-web/src/profile.ts`), so a consent-denied visitor writes nothing and is **uncounted**.

**Add (optional mode):** a PostHog `on_reject`-style cookieless mode — for consent-denied / pre-consent visitors, derive `distinct_id` server-side as `hash(project_id, daily_salt, ip, ua, host)` (reusing the transient IP from §4.4), rotate+destroy the salt daily so the hash is anonymous not pseudonymous, and **upgrade** to the UUID profile on opt-in. Keeps the UUID model as the default (Pug is product analytics — anonymous retention + anon→identified merge must survive); this is opt-in only. `identify()` blocked while cookieless; these profiles are synthetic and outside DSAR/merge scope. Full design, salt lifecycle, and open questions in the scope doc.

### P2 — Security & residency (our processor duties)

#### 4.8 Security hardening ⚠️ — GDPR Art. 32 / DPDP §8(5) (and DPDP makes the processor liable for breaches)

- **Verify** TLS is actually terminated at the edge — infra sets `http-redirect-https: false` and OTLP `insecure: true`; confirm Cloudflare/LB fronts everything with HTTPS.
- **Confirm** encryption at rest on the Hetzner volumes (PostgreSQL, ClickHouse, NATS).
- **Scrub PII from telemetry** — RPC/SQL traces can carry IDs/emails into SigNoz (`docs/architecture/telemetry.md`).
- Write a **breach-notification runbook** — DPDP requires notifying the Data Protection Board *and* affected principals; GDPR is 72h to the supervisory authority.

#### 4.9 Data residency / cross-border ⚠️

**Today:** single-region **Hetzner Helsinki** (EU). This is genuinely *fine* for GDPR (EU data stays in the EU). For **DPDP** it's nuanced: India uses a blacklist/transfer model, and Indian customers often *expect* India residency.

**Decide the story:** either stand up an India region with per-org region pinning, or publish a clear transfer position + SCCs. Biggest infra lift — treat as strategic, not a quick fix. (Single-region is also an Art. 32 *availability* risk — backups to S3 are listed as open items in the infra repo's `data-architecture.md`.)

---

## 5. Non-code deliverables (what customers actually sign and rely on)

- **DPA template** + a public **sub-processor list** — note **Cloudflare** sees raw IPs and is US-based (a disclosed sub-processor and a transfer point); likewise Hetzner and any email provider.
- **Records of Processing (RoPA, GDPR Art. 30)** — §3 above is ~80% of this; keep it current here.
- **DSAR audit log** — delivered by 4.1's request table; we must show *what* was deleted/exported, *when*.
- **Consent-mode + cookie/localStorage docs** for customers (ties to 4.7).

---

## 6. Suggested sequencing

Retire the most legal risk per unit of effort:

1. ✅ **4.4 Stop storing raw IP** — done in [PR #15](https://github.com/pug-sh/pug/pull/15) (pending merge).
2. **4.1 Erasure-reaches-events** ← **next up** — the one unambiguous failure; the alias-resolution + worker is the real engineering.
3. **4.5 Retention/TTL** and **4.2 Export** — reuse 4.1's alias machinery.

Everything else layers on after those three.

---

## 7. Cross-references

- Event enrichment / IP / geo → [`../architecture/ingestion.md`](../architecture/ingestion.md)
- Events table, dedup, partitioning → [`../architecture/clickhouse.md`](../architecture/clickhouse.md)
- Profiles model, soft-delete, aliases → [`../architecture/profiles.md`](../architecture/profiles.md)
- Telemetry / PII-in-traces → [`../architecture/telemetry.md`](../architecture/telemetry.md)
- SDK consent / autocapture → `../../../sdk-web/CLAUDE.md`

## Appendix — regulation → feature map

| Requirement | GDPR | DPDP | Item |
| --- | --- | --- | --- |
| Right to erasure | Art. 17 | §12 | 4.1 |
| Right to access | Art. 15 | §11 | 4.2 |
| Data portability | Art. 20 | — | 4.2 |
| Rectification | Art. 16 | §11 | 4.3 |
| Storage limitation / erase-when-done | Art. 5(1)(e) | §8(7) | 4.4 ✅, 4.5 |
| Data minimisation | Art. 5(1)(c) | §6 | 4.4 ✅, 4.6 |
| Consent & withdrawal | Art. 6/7, ePrivacy | §6, §7 | 4.6, 4.7 |
| Security safeguards | Art. 32 | §8(5) | 4.8 |
| Breach notification | Art. 33/34 | §8(6) | 4.8 |
| Cross-border transfer | Ch. V | §16 | 4.9 |
| Processor terms / sub-processors | Art. 28 | §8(2) | §5 |
| Accountability / records | Art. 5(2), 30 | §8(1) | 4.1 (audit), §5 |
