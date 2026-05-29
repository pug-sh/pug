-- +goose Up

CREATE TABLE IF NOT EXISTS dashboard_session_rollup (
    project_id              String,
    kind                    LowCardinality(String),
    session_id              UUID,
    start_state             AggregateFunction(min, DateTime64(3)),
    end_state               AggregateFunction(max, DateTime64(3)),
    event_count_state       AggregateFunction(count),
    entry_url_state         AggregateFunction(argMin, String, DateTime64(3)),
    exit_url_state          AggregateFunction(argMax, String, DateTime64(3)),
    entry_country_state     AggregateFunction(argMin, String, DateTime64(3)),
    exit_country_state      AggregateFunction(argMax, String, DateTime64(3)),
    entry_region_state      AggregateFunction(argMin, String, DateTime64(3)),
    exit_region_state       AggregateFunction(argMax, String, DateTime64(3)),
    entry_city_state        AggregateFunction(argMin, String, DateTime64(3)),
    exit_city_state         AggregateFunction(argMax, String, DateTime64(3)),
    entry_os_state          AggregateFunction(argMin, String, DateTime64(3)),
    exit_os_state           AggregateFunction(argMax, String, DateTime64(3)),
    entry_browser_state     AggregateFunction(argMin, String, DateTime64(3)),
    exit_browser_state      AggregateFunction(argMax, String, DateTime64(3)),
    entry_device_state      AggregateFunction(argMin, String, DateTime64(3)),
    exit_device_state       AggregateFunction(argMax, String, DateTime64(3)),
    entry_platform_state    AggregateFunction(argMin, String, DateTime64(3)),
    exit_platform_state     AggregateFunction(argMax, String, DateTime64(3)),
    entry_utm_source_state  AggregateFunction(argMin, String, DateTime64(3)),
    exit_utm_source_state   AggregateFunction(argMax, String, DateTime64(3)),
    entry_utm_medium_state  AggregateFunction(argMin, String, DateTime64(3)),
    exit_utm_medium_state   AggregateFunction(argMax, String, DateTime64(3)),
    entry_utm_campaign_state AggregateFunction(argMin, String, DateTime64(3)),
    exit_utm_campaign_state  AggregateFunction(argMax, String, DateTime64(3))
) ENGINE = AggregatingMergeTree
ORDER BY (project_id, kind, session_id);

-- Session-grain rollup for dashboard session insights. This deliberately stores
-- mergeable per-session aggregate states instead of daily rows: a session can
-- arrive across multiple inserts, and min/max/argMin/argMax/count states can be
-- merged by session_id at read time without freezing an incorrect start day,
-- duration, entry page, or exit page.

CREATE MATERIALIZED VIEW IF NOT EXISTS dashboard_session_rollup_mv
TO dashboard_session_rollup AS
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

-- Backfill history. The MV only captures inserts after creation; this one-time
-- load uses the same state shape so AggregatingMergeTree can merge it with
-- future incremental states.
INSERT INTO dashboard_session_rollup
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

-- +goose Down

DROP TABLE IF EXISTS dashboard_session_rollup_mv;
DROP TABLE IF EXISTS dashboard_session_rollup;