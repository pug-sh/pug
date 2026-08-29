-- +goose Up

-- Key the per-distinct_id activity rollup by `bot` so the profiles read path can
-- honour include_bots (docs/architecture/bot-detection.md). Migration 012 keyed
-- only the two dashboard rollups, so a crawler still materialized as a derived
-- anonymous person with unfiltered counts under a bot-filtered profile page.
-- Joins ORDER BY in the same ALTER with no DEFAULT, the 011/012 way (code 36).
-- Unlike the session rollup this table is keyed per distinct_id, not per
-- session, so a row-level `bot = 0` is exact; an id with both kinds of traffic
-- splits into two rows that merge back when bots are included.
--
-- NOT backfilled, deliberately. 012 shipped tagging (v0.0.19) without keying
-- this table, so rows the MV wrote between that deploy and this one aggregated
-- without `bot` and read as 0 afterwards — a crawler active in that window
-- keeps listing as a person. AggregatingMergeTree never rewrites states, so the
-- window is permanent; it is ~1 day of traffic and accepted rather than rebuilt.
-- `SELECT count() FROM events WHERE bot = 1 AND occur_time < '<013 deploy>'`
-- sizes it if that ever needs revisiting.

ALTER TABLE distinct_id_activity_states
    ADD COLUMN IF NOT EXISTS bot UInt8,
    MODIFY ORDER BY (project_id, distinct_id, bot);

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
    argMaxState(city, occur_time)            AS latest_city_state,
    toUInt8(bot)                             AS bot
FROM events
WHERE NOT startsWith(distinct_id, 'cookieless-')
GROUP BY project_id, distinct_id, bot;

-- +goose Down

-- Restore the 011 activity MV query. DEV ONLY — do not run this in production.
-- The `bot` key column cannot be dropped (it is part of the sorting key), so
-- `states.bot = 0` keeps resolving and every read keeps returning 200 while the
-- MV stops emitting `bot` and new rows take the type default 0. Bot exclusion on
-- the profiles surface silently becomes a no-op for all traffic from that point,
-- with no error anywhere to notice it by.

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
