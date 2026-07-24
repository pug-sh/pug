-- +goose Up

-- Extends the daily event rollup (migration 006) with the ten web-analytics
-- dimensions. Three steps, ORDER LOAD-BEARING on a live system:
--
--   1. MODIFY QUERY on the MV — never DROP->CREATE, which would lose ALL dims
--      (including $__total__) for events inserted in the gap. The full
--      21-tuple ARRAY JOIN is restated; dim exprs read promoted columns only
--      (TestMigration011PromotedDimExprsMatch, which parses the migration that
--      currently defines the MV) and must match
--      AutoPropertyProjectionFor exactly.
--
--   2. DELETE the NEW dim_names, so step 3 is re-runnable. Only a no-op on a
--      first run; see the statement's own comment for why a partial INSERT
--      plus a re-run would otherwise double cnt forever.
--
--   3. Delta backfill INSERT restricted to the NEW dim_names. New EAV key
--      rows are disjoint from every existing row (dim_name is in the ORDER BY
--      key), so there is no merge hazard — and existing dims must NOT be
--      re-inserted (a full-list backfill would double cnt for the old dims).
--
-- Deriving the promoted columns is 008's job alone, and this file deliberately
-- does not repeat it: migrations run as one ArgoCD PreSync hook
-- (`pug clickhouse migrate`), so 008's mutation lands seconds before step 3
-- reads `events`, and a second copy here would be a full-table rewrite of every
-- partition to derive nothing. Nor is there a deploy window to catch up: PreSync
-- completes before any pod rolls, so no new binary has written a row yet. A
-- failing step 3 retries automatically (PreSync backoffLimit + the Application's
-- retry limit) minutes apart, and aborts the Sync so no rollout happens — a gap
-- long enough to strand underived rows means a human is already intervening, and
-- the repair is 008's mutation followed by steps 2+3. That is the one repair
-- procedure for every case, the rollout window included; see the deploy runbook
-- in docs/architecture/web-analytics.md.
--
-- Accepted accuracy tradeoff: events inserted between step 1 taking effect
-- and step 3's snapshot are counted twice into cnt for the NEW dims only
-- (uniq_state is idempotent) — the same bounded class as the documented
-- redelivery over-count in migration 006, on dims with zero historical
-- baseline. A cutoff cannot fully close this (an insert in flight during
-- MODIFY QUERY is ambiguous either way), so it is accepted and documented.

ALTER TABLE dashboard_event_rollup_daily_mv MODIFY QUERY
SELECT
    project_id,
    toDate(occur_time) AS day,
    kind,
    dim.1 AS dim_name,
    dim.2 AS dim_value,
    count() AS cnt,
    uniqState(distinct_id) AS uniq_state
FROM events
ARRAY JOIN [
    ('$__total__', ''),
    ('$country',        coalesce(country, '')),
    ('$region',         coalesce(region, '')),
    ('$city',           coalesce(city, '')),
    ('$os',             coalesce(os, '')),
    ('$browser',        coalesce(browser, '')),
    ('$device',         coalesce(device, '')),
    ('$platform',       coalesce(platform, '')),
    ('$utmSource',      coalesce(utm_source, '')),
    ('$utmMedium',      coalesce(utm_medium, '')),
    ('$utmCampaign',    coalesce(utm_campaign, '')),
    ('$pathname',       coalesce(pathname, '')),
    ('$hostname',       coalesce(hostname, '')),
    ('$referrerDomain', coalesce(referrer_domain, '')),
    ('$channel',        coalesce(channel, '')),
    ('$locale',         coalesce(locale, '')),
    ('$screenSize',     coalesce(screen_size, '')),
    ('$utmTerm',        coalesce(utm_term, '')),
    ('$utmContent',     coalesce(utm_content, '')),
    ('$browserVersion', coalesce(browser_version, '')),
    ('$osVersion',      coalesce(os_version, ''))
] AS dim
GROUP BY project_id, day, kind, dim_name, dim_value;

-- Re-run guard, and the reason step 3 is safe to retry. `cnt` is
-- SimpleAggregateFunction(sum), so AggregatingMergeTree SUMS duplicate keys:
-- an INSERT ... SELECT that dies partway (max_execution_time, memory) cannot
-- roll back, leaves its parts behind, and goose never records version 9 — so
-- the natural `pug clickhouse migrate` re-run would lay a second full copy on
-- top and permanently inflate cnt for every new dim, with UNIQUE_USERS still
-- correct so the result looks plausible rather than broken. Deleting first
-- makes the pair idempotent: a no-op on the first run (no rows carry these
-- dim_names until the INSERT below), and on a retry the INSERT re-derives
-- them from `events`, the source of truth. Keep the list equal to
-- eventRollupDims009 — TestMigration009BackfillDeleteCoversNewDims.
ALTER TABLE dashboard_event_rollup_daily DELETE
WHERE dim_name IN ('$pathname', '$hostname', '$referrerDomain', '$channel', '$locale', '$screenSize', '$utmTerm', '$utmContent', '$browserVersion', '$osVersion')
SETTINGS mutations_sync = 2;

-- Delta backfill: NEW dims only (see header). Same state shape as the MV so
-- AggregatingMergeTree merges it with incremental states.
INSERT INTO dashboard_event_rollup_daily
SELECT
    project_id,
    toDate(occur_time) AS day,
    kind,
    dim.1 AS dim_name,
    dim.2 AS dim_value,
    count() AS cnt,
    uniqState(distinct_id) AS uniq_state
FROM events
ARRAY JOIN [
    ('$pathname',       coalesce(pathname, '')),
    ('$hostname',       coalesce(hostname, '')),
    ('$referrerDomain', coalesce(referrer_domain, '')),
    ('$channel',        coalesce(channel, '')),
    ('$locale',         coalesce(locale, '')),
    ('$screenSize',     coalesce(screen_size, '')),
    ('$utmTerm',        coalesce(utm_term, '')),
    ('$utmContent',     coalesce(utm_content, '')),
    ('$browserVersion', coalesce(browser_version, '')),
    ('$osVersion',      coalesce(os_version, ''))
] AS dim
GROUP BY project_id, day, kind, dim_name, dim_value;

-- +goose Down

-- Restore the migration-006 MV query and drop the new-dim rows. The DELETE is
-- best-effort hygiene for dev; nothing reads dims that are no longer in
-- materializedDims.
ALTER TABLE dashboard_event_rollup_daily_mv MODIFY QUERY
SELECT
    project_id,
    toDate(occur_time) AS day,
    kind,
    dim.1 AS dim_name,
    dim.2 AS dim_value,
    count() AS cnt,
    uniqState(distinct_id) AS uniq_state
FROM events
ARRAY JOIN [
    ('$__total__', ''),
    ('$country',     coalesce(country, '')),
    ('$region',      coalesce(region, '')),
    ('$city',        coalesce(city, '')),
    ('$os',          coalesce(os, '')),
    ('$browser',     coalesce(browser, '')),
    ('$device',      coalesce(device, '')),
    ('$platform',    coalesce(platform, '')),
    ('$utmSource',   coalesce(utm_source, '')),
    ('$utmMedium',   coalesce(utm_medium, '')),
    ('$utmCampaign', coalesce(utm_campaign, ''))
] AS dim
GROUP BY project_id, day, kind, dim_name, dim_value;

ALTER TABLE dashboard_event_rollup_daily DELETE
WHERE dim_name IN ('$pathname', '$hostname', '$referrerDomain', '$channel', '$locale', '$screenSize', '$utmTerm', '$utmContent', '$browserVersion', '$osVersion')
SETTINGS mutations_sync = 2;
