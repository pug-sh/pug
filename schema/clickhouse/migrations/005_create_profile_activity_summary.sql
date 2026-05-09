-- +goose Up

CREATE TABLE IF NOT EXISTS distinct_id_activity_states (
    project_id                    LowCardinality(String),
    distinct_id                   String,
    first_seen_state              AggregateFunction(min, DateTime64(3)),
    last_seen_state               AggregateFunction(max, DateTime64(3)),
    total_events_state            AggregateFunction(count),
    pageviews_state               AggregateFunction(sum, UInt64),
    sessions_state                AggregateFunction(uniq, UUID),
    latest_browser_state          AggregateFunction(argMax, String, DateTime64(3)),
    latest_browser_version_state  AggregateFunction(argMax, String, DateTime64(3)),
    latest_os_state               AggregateFunction(argMax, String, DateTime64(3)),
    latest_os_version_state       AggregateFunction(argMax, String, DateTime64(3)),
    latest_device_state           AggregateFunction(argMax, String, DateTime64(3)),
    latest_country_state          AggregateFunction(argMax, String, DateTime64(3)),
    latest_region_state           AggregateFunction(argMax, String, DateTime64(3)),
    latest_city_state             AggregateFunction(argMax, String, DateTime64(3))
) ENGINE = AggregatingMergeTree()
ORDER BY (project_id, distinct_id);

CREATE MATERIALIZED VIEW IF NOT EXISTS distinct_id_activity_states_mv
TO distinct_id_activity_states AS
SELECT
    project_id,
    distinct_id,
    minState(occur_time)                     AS first_seen_state,
    maxState(occur_time)                     AS last_seen_state,
    countState()                             AS total_events_state,
    sumState(toUInt64(kind = 'page_view'))   AS pageviews_state,
    uniqState(session_id)                    AS sessions_state,
    argMaxState(coalesce(CAST(auto_properties['$browser'] AS Nullable(String)), ''), occur_time) AS latest_browser_state,
    argMaxState(coalesce(CAST(auto_properties['$browserVersion'] AS Nullable(String)), ''), occur_time) AS latest_browser_version_state,
    argMaxState(coalesce(CAST(auto_properties['$os'] AS Nullable(String)), ''), occur_time) AS latest_os_state,
    argMaxState(coalesce(CAST(auto_properties['$osVersion'] AS Nullable(String)), ''), occur_time) AS latest_os_version_state,
    argMaxState(coalesce(CAST(auto_properties['$device'] AS Nullable(String)), ''), occur_time) AS latest_device_state,
    argMaxState(coalesce(CAST(auto_properties['$country'] AS Nullable(String)), ''), occur_time) AS latest_country_state,
    argMaxState(coalesce(CAST(auto_properties['$region'] AS Nullable(String)), ''), occur_time) AS latest_region_state,
    argMaxState(coalesce(CAST(auto_properties['$city'] AS Nullable(String)), ''), occur_time) AS latest_city_state
FROM events
GROUP BY project_id, distinct_id;

-- +goose Down
DROP VIEW IF EXISTS distinct_id_activity_states_mv;
DROP TABLE IF EXISTS distinct_id_activity_states;
