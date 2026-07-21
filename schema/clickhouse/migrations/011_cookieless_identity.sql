-- +goose Up

-- Cookieless visitor identity (docs/architecture/ingestion.md): ingest derives
-- daily-rotating ids prefixed 'cookieless-' for no-consent visitors. The
-- prefix in distinct_id is the single source of truth — no flag column on
-- events that could disagree with the id. Two MV changes keep those ids out of
-- identity surfaces; the literal must equal cookieless.IDPrefix
-- (TestMigration011CookielessPrefixMatchesGo).
--
--   1. distinct_id_activity_states_mv: WHERE NOT startsWith(...) — a
--      cookieless id must never mint a derived anonymous person (each daily
--      rotation would create a ghost). MODIFY QUERY, never DROP->CREATE (a
--      gap would silently lose activity for events inserted in between).
--   2. dashboard_event_rollup_daily: a `cookieless` key column computed from
--      the prefix, so user-count reads exclude with WHERE cookieless = 0
--      while TOTAL reads (no predicate) keep counting all traffic. The column
--      joins the ORDER BY in the same ALTER that adds it — the only position
--      ClickHouse allows for a new key column — and deliberately carries NO
--      DEFAULT expression: ClickHouse forbids defaulted columns in a new
--      sorting key (code 36), and the bare UInt8 makes existing rows read the
--      type default 0, which is historically exact — no cookieless rows
--      predate this migration, so THERE IS NOTHING TO BACKFILL (unlike
--      008-010).
--
--      That rests on TWO assumptions, and only the first is self-evident:
--      (a) ingest minted no such ids before this change, and (b) no tenant
--      ever sent a distinct_id carrying this prefix of their own accord.
--      The reserved-prefix rule (batch.distinct_id_reserved_prefix) ships in
--      the same branch as this migration, so (b) went UNENFORCED for all
--      prior history. Such a row reads cookieless=0 here while the raw path
--      excludes it by prefix — a silent rollup/raw parity break, bounded to
--      tenants who happened to pick this prefix. Before relying on parity for
--      an existing deployment, check:
--        SELECT count() FROM events
--         WHERE startsWith(distinct_id, <cookieless.IDPrefix>)
--           AND occur_time < [011 deploy time]
--
-- The session rollup is deliberately untouched: session metrics always count
-- all traffic (spec Decision 1), and session builders never read distinct_id.

ALTER TABLE distinct_id_activity_states_mv MODIFY QUERY
SELECT
    project_id,
    distinct_id,
    minState(occur_time)                     AS first_seen_state,
    maxState(occur_time)                     AS last_seen_state,
    countState()                             AS total_events_state,
    sumState(toUInt64(kind = 'page_view'))   AS pageviews_state,
    uniqState(session_id)                    AS sessions_state,
    argMaxState(browser, occur_time)         AS latest_browser_state,
    argMaxState(browser_version, occur_time) AS latest_browser_version_state,
    argMaxState(os, occur_time)              AS latest_os_state,
    argMaxState(os_version, occur_time)      AS latest_os_version_state,
    argMaxState(device, occur_time)          AS latest_device_state,
    argMaxState(country, occur_time)         AS latest_country_state,
    argMaxState(region, occur_time)          AS latest_region_state,
    argMaxState(city, occur_time)            AS latest_city_state
FROM events
WHERE NOT startsWith(distinct_id, 'cookieless-')
GROUP BY project_id, distinct_id;

ALTER TABLE dashboard_event_rollup_daily
    ADD COLUMN IF NOT EXISTS cookieless UInt8,
    MODIFY ORDER BY (project_id, kind, dim_name, day, dim_value, cookieless);

ALTER TABLE dashboard_event_rollup_daily_mv MODIFY QUERY
SELECT
    project_id,
    toDate(occur_time) AS day,
    kind,
    dim.1 AS dim_name,
    dim.2 AS dim_value,
    count() AS cnt,
    uniqState(distinct_id) AS uniq_state,
    toUInt8(startsWith(distinct_id, 'cookieless-')) AS cookieless
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
GROUP BY project_id, day, kind, dim_name, dim_value, cookieless;

-- +goose Down

-- Restore the 005 activity MV (no WHERE) and the 009 rollup MV query. The
-- cookieless key column cannot be dropped (it is part of the sorting key);
-- after this Down, the MV stops emitting it and new rows take DEFAULT 0 —
-- fine for dev, where down migrations run against disposable databases.

ALTER TABLE distinct_id_activity_states_mv MODIFY QUERY
SELECT
    project_id,
    distinct_id,
    minState(occur_time)                     AS first_seen_state,
    maxState(occur_time)                     AS last_seen_state,
    countState()                             AS total_events_state,
    sumState(toUInt64(kind = 'page_view'))   AS pageviews_state,
    uniqState(session_id)                    AS sessions_state,
    argMaxState(browser, occur_time)         AS latest_browser_state,
    argMaxState(browser_version, occur_time) AS latest_browser_version_state,
    argMaxState(os, occur_time)              AS latest_os_state,
    argMaxState(os_version, occur_time)      AS latest_os_version_state,
    argMaxState(device, occur_time)          AS latest_device_state,
    argMaxState(country, occur_time)         AS latest_country_state,
    argMaxState(region, occur_time)          AS latest_region_state,
    argMaxState(city, occur_time)            AS latest_city_state
FROM events
GROUP BY project_id, distinct_id;

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
