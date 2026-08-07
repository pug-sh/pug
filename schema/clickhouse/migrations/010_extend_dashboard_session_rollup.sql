-- +goose Up

-- Extends the session-grain rollup (migration 007) with entry/exit state
-- pairs for the five web-analytics session dimensions ($pathname powers
-- Entry/Exit pages; referrer_domain/channel/utm_term/utm_content power
-- first-touch acquisition panels). Three steps, ORDER LOAD-BEARING:
--
--   1. ADD COLUMN for the ten AggregateFunction states (metadata-only).
--      Existing rows read the implicit default — the EMPTY aggregate state,
--      which is the merge identity — so history is untouched.
--
--   2. MODIFY QUERY restating all 32 argMin/argMax states (+ start/end/count)
--      with the inner UNION ALL projecting the new columns. Never
--      DROP->CREATE (events inserted in the gap would lose ALL states).
--
--   3. Partial-column backfill INSERT listing ONLY the key columns + the ten
--      new states. Omitted AggregateFunction columns take their default —
--      the empty state, the merge identity — so event_count_state /
--      start/end and the existing entry/exit states are untouched and
--      bounce/duration cannot double. This is the one backfill that is
--      silently catastrophic if done naively (re-inserting countState()
--      doubles every session's event count); the merge-identity behavior is
--      pinned by TestIntegrationWebAnalytics/session_rollup_partial_insert_merge_identity.
--      The argMin/argMax states themselves are idempotent under duplicate
--      merge, so the MODIFY->INSERT overlap is harmless here.
--
-- LowCardinality source columns go through toString() (AggregateFunction
-- argMin/argMax is declared over plain String); pathname is a plain String
-- column and stays bare — the migration-007 convention, pinned by
-- TestMigration010SessionRollupDimExprsMatch.

ALTER TABLE dashboard_session_rollup
    ADD COLUMN IF NOT EXISTS entry_pathname_state        AggregateFunction(argMin, String, DateTime64(3)),
    ADD COLUMN IF NOT EXISTS exit_pathname_state         AggregateFunction(argMax, String, DateTime64(3)),
    ADD COLUMN IF NOT EXISTS entry_referrer_domain_state AggregateFunction(argMin, String, DateTime64(3)),
    ADD COLUMN IF NOT EXISTS exit_referrer_domain_state  AggregateFunction(argMax, String, DateTime64(3)),
    ADD COLUMN IF NOT EXISTS entry_channel_state         AggregateFunction(argMin, String, DateTime64(3)),
    ADD COLUMN IF NOT EXISTS exit_channel_state          AggregateFunction(argMax, String, DateTime64(3)),
    ADD COLUMN IF NOT EXISTS entry_utm_term_state        AggregateFunction(argMin, String, DateTime64(3)),
    ADD COLUMN IF NOT EXISTS exit_utm_term_state         AggregateFunction(argMax, String, DateTime64(3)),
    ADD COLUMN IF NOT EXISTS entry_utm_content_state     AggregateFunction(argMin, String, DateTime64(3)),
    ADD COLUMN IF NOT EXISTS exit_utm_content_state      AggregateFunction(argMax, String, DateTime64(3));

ALTER TABLE dashboard_session_rollup_mv MODIFY QUERY
SELECT
    project_id,
    kind,
    session_id,
    minState(occur_time) AS start_state,
    maxState(occur_time) AS end_state,
    countState() AS event_count_state,
    argMinState(url, occur_time) AS entry_url_state,
    argMaxState(url, occur_time) AS exit_url_state,
    argMinState(toString(country), occur_time) AS entry_country_state,
    argMaxState(toString(country), occur_time) AS exit_country_state,
    argMinState(toString(region), occur_time) AS entry_region_state,
    argMaxState(toString(region), occur_time) AS exit_region_state,
    argMinState(city, occur_time) AS entry_city_state,
    argMaxState(city, occur_time) AS exit_city_state,
    argMinState(toString(os), occur_time) AS entry_os_state,
    argMaxState(toString(os), occur_time) AS exit_os_state,
    argMinState(toString(browser), occur_time) AS entry_browser_state,
    argMaxState(toString(browser), occur_time) AS exit_browser_state,
    argMinState(toString(device), occur_time) AS entry_device_state,
    argMaxState(toString(device), occur_time) AS exit_device_state,
    argMinState(toString(platform), occur_time) AS entry_platform_state,
    argMaxState(toString(platform), occur_time) AS exit_platform_state,
    argMinState(toString(utm_source), occur_time) AS entry_utm_source_state,
    argMaxState(toString(utm_source), occur_time) AS exit_utm_source_state,
    argMinState(toString(utm_medium), occur_time) AS entry_utm_medium_state,
    argMaxState(toString(utm_medium), occur_time) AS exit_utm_medium_state,
    argMinState(toString(utm_campaign), occur_time) AS entry_utm_campaign_state,
    argMaxState(toString(utm_campaign), occur_time) AS exit_utm_campaign_state,
    argMinState(pathname, occur_time) AS entry_pathname_state,
    argMaxState(pathname, occur_time) AS exit_pathname_state,
    argMinState(toString(referrer_domain), occur_time) AS entry_referrer_domain_state,
    argMaxState(toString(referrer_domain), occur_time) AS exit_referrer_domain_state,
    argMinState(toString(channel), occur_time) AS entry_channel_state,
    argMaxState(toString(channel), occur_time) AS exit_channel_state,
    argMinState(toString(utm_term), occur_time) AS entry_utm_term_state,
    argMaxState(toString(utm_term), occur_time) AS exit_utm_term_state,
    argMinState(toString(utm_content), occur_time) AS entry_utm_content_state,
    argMaxState(toString(utm_content), occur_time) AS exit_utm_content_state
FROM (
    SELECT
        project_id,
        session_id,
        '' AS kind,
        occur_time,
        url,
        country,
        region,
        city,
        os,
        browser,
        device,
        platform,
        utm_source,
        utm_medium,
        utm_campaign,
        pathname,
        referrer_domain,
        channel,
        utm_term,
        utm_content
    FROM events

    UNION ALL

    SELECT
        project_id,
        session_id,
        kind,
        occur_time,
        url,
        country,
        region,
        city,
        os,
        browser,
        device,
        platform,
        utm_source,
        utm_medium,
        utm_campaign,
        pathname,
        referrer_domain,
        channel,
        utm_term,
        utm_content
    FROM events
) AS scoped_events
GROUP BY project_id, kind, session_id;

-- Partial-column backfill: key columns + the ten NEW states only (see header).
INSERT INTO dashboard_session_rollup (
    project_id,
    kind,
    session_id,
    entry_pathname_state,
    exit_pathname_state,
    entry_referrer_domain_state,
    exit_referrer_domain_state,
    entry_channel_state,
    exit_channel_state,
    entry_utm_term_state,
    exit_utm_term_state,
    entry_utm_content_state,
    exit_utm_content_state
)
SELECT
    project_id,
    kind,
    session_id,
    argMinState(pathname, occur_time) AS entry_pathname_state,
    argMaxState(pathname, occur_time) AS exit_pathname_state,
    argMinState(toString(referrer_domain), occur_time) AS entry_referrer_domain_state,
    argMaxState(toString(referrer_domain), occur_time) AS exit_referrer_domain_state,
    argMinState(toString(channel), occur_time) AS entry_channel_state,
    argMaxState(toString(channel), occur_time) AS exit_channel_state,
    argMinState(toString(utm_term), occur_time) AS entry_utm_term_state,
    argMaxState(toString(utm_term), occur_time) AS exit_utm_term_state,
    argMinState(toString(utm_content), occur_time) AS entry_utm_content_state,
    argMaxState(toString(utm_content), occur_time) AS exit_utm_content_state
FROM (
    SELECT
        project_id,
        session_id,
        '' AS kind,
        occur_time,
        pathname,
        referrer_domain,
        channel,
        utm_term,
        utm_content
    FROM events

    UNION ALL

    SELECT
        project_id,
        session_id,
        kind,
        occur_time,
        pathname,
        referrer_domain,
        channel,
        utm_term,
        utm_content
    FROM events
) AS scoped_events
GROUP BY project_id, kind, session_id;

-- +goose Down

-- Restore the migration-007 MV query, then drop the new state columns.
ALTER TABLE dashboard_session_rollup_mv MODIFY QUERY
SELECT
    project_id,
    kind,
    session_id,
    minState(occur_time) AS start_state,
    maxState(occur_time) AS end_state,
    countState() AS event_count_state,
    argMinState(url, occur_time) AS entry_url_state,
    argMaxState(url, occur_time) AS exit_url_state,
    argMinState(toString(country), occur_time) AS entry_country_state,
    argMaxState(toString(country), occur_time) AS exit_country_state,
    argMinState(toString(region), occur_time) AS entry_region_state,
    argMaxState(toString(region), occur_time) AS exit_region_state,
    argMinState(city, occur_time) AS entry_city_state,
    argMaxState(city, occur_time) AS exit_city_state,
    argMinState(toString(os), occur_time) AS entry_os_state,
    argMaxState(toString(os), occur_time) AS exit_os_state,
    argMinState(toString(browser), occur_time) AS entry_browser_state,
    argMaxState(toString(browser), occur_time) AS exit_browser_state,
    argMinState(toString(device), occur_time) AS entry_device_state,
    argMaxState(toString(device), occur_time) AS exit_device_state,
    argMinState(toString(platform), occur_time) AS entry_platform_state,
    argMaxState(toString(platform), occur_time) AS exit_platform_state,
    argMinState(toString(utm_source), occur_time) AS entry_utm_source_state,
    argMaxState(toString(utm_source), occur_time) AS exit_utm_source_state,
    argMinState(toString(utm_medium), occur_time) AS entry_utm_medium_state,
    argMaxState(toString(utm_medium), occur_time) AS exit_utm_medium_state,
    argMinState(toString(utm_campaign), occur_time) AS entry_utm_campaign_state,
    argMaxState(toString(utm_campaign), occur_time) AS exit_utm_campaign_state
FROM (
    SELECT
        project_id,
        session_id,
        '' AS kind,
        occur_time,
        url,
        country,
        region,
        city,
        os,
        browser,
        device,
        platform,
        utm_source,
        utm_medium,
        utm_campaign
    FROM events

    UNION ALL

    SELECT
        project_id,
        session_id,
        kind,
        occur_time,
        url,
        country,
        region,
        city,
        os,
        browser,
        device,
        platform,
        utm_source,
        utm_medium,
        utm_campaign
    FROM events
) AS scoped_events
GROUP BY project_id, kind, session_id;

ALTER TABLE dashboard_session_rollup
    DROP COLUMN IF EXISTS entry_pathname_state,
    DROP COLUMN IF EXISTS exit_pathname_state,
    DROP COLUMN IF EXISTS entry_referrer_domain_state,
    DROP COLUMN IF EXISTS exit_referrer_domain_state,
    DROP COLUMN IF EXISTS entry_channel_state,
    DROP COLUMN IF EXISTS exit_channel_state,
    DROP COLUMN IF EXISTS entry_utm_term_state,
    DROP COLUMN IF EXISTS exit_utm_term_state,
    DROP COLUMN IF EXISTS entry_utm_content_state,
    DROP COLUMN IF EXISTS exit_utm_content_state;
