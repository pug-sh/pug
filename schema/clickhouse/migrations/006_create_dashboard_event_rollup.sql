-- +goose Up

CREATE TABLE IF NOT EXISTS dashboard_event_rollup_daily (
    project_id  String,
    day         Date,
    kind        LowCardinality(String),
    dim_name    LowCardinality(String),
    dim_value   String,
    cnt         SimpleAggregateFunction(sum, UInt64),
    uniq_state  AggregateFunction(uniq, String)
) ENGINE = AggregatingMergeTree
ORDER BY (project_id, kind, dim_name, day, dim_value);

CREATE MATERIALIZED VIEW IF NOT EXISTS dashboard_event_rollup_daily_mv
TO dashboard_event_rollup_daily AS
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
    ('$country',     coalesce(nullIf(CAST(auto_properties['$country']     AS Nullable(String)), ''), CAST(custom_properties['$country']     AS Nullable(String)), '')),
    ('$region',      coalesce(nullIf(CAST(auto_properties['$region']      AS Nullable(String)), ''), CAST(custom_properties['$region']      AS Nullable(String)), '')),
    ('$city',        coalesce(nullIf(CAST(auto_properties['$city']        AS Nullable(String)), ''), CAST(custom_properties['$city']        AS Nullable(String)), '')),
    ('$os',          coalesce(nullIf(CAST(auto_properties['$os']          AS Nullable(String)), ''), CAST(custom_properties['$os']          AS Nullable(String)), '')),
    ('$browser',     coalesce(nullIf(CAST(auto_properties['$browser']     AS Nullable(String)), ''), CAST(custom_properties['$browser']     AS Nullable(String)), '')),
    ('$device',      coalesce(nullIf(CAST(auto_properties['$device']      AS Nullable(String)), ''), CAST(custom_properties['$device']      AS Nullable(String)), '')),
    ('$platform',    coalesce(nullIf(CAST(auto_properties['$platform']    AS Nullable(String)), ''), CAST(custom_properties['$platform']    AS Nullable(String)), '')),
    ('$utmSource',   coalesce(nullIf(CAST(auto_properties['$utmSource']   AS Nullable(String)), ''), CAST(custom_properties['$utmSource']   AS Nullable(String)), '')),
    ('$utmMedium',   coalesce(nullIf(CAST(auto_properties['$utmMedium']   AS Nullable(String)), ''), CAST(custom_properties['$utmMedium']   AS Nullable(String)), '')),
    ('$utmCampaign', coalesce(nullIf(CAST(auto_properties['$utmCampaign'] AS Nullable(String)), ''), CAST(custom_properties['$utmCampaign'] AS Nullable(String)), ''))
] AS dim
GROUP BY project_id, day, kind, dim_name, dim_value;

-- Backfill history. The MV only captures inserts after it is created, so existing
-- events must be loaded once. Safe here: pre-deployment, disposable data, no live
-- ingestion during migration. If this ever runs against live ingestion, add
-- `WHERE occur_time < <mv_creation_cutoff>` so MV + backfill do not overlap.
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
    ('$__total__', ''),
    ('$country',     coalesce(nullIf(CAST(auto_properties['$country']     AS Nullable(String)), ''), CAST(custom_properties['$country']     AS Nullable(String)), '')),
    ('$region',      coalesce(nullIf(CAST(auto_properties['$region']      AS Nullable(String)), ''), CAST(custom_properties['$region']      AS Nullable(String)), '')),
    ('$city',        coalesce(nullIf(CAST(auto_properties['$city']        AS Nullable(String)), ''), CAST(custom_properties['$city']        AS Nullable(String)), '')),
    ('$os',          coalesce(nullIf(CAST(auto_properties['$os']          AS Nullable(String)), ''), CAST(custom_properties['$os']          AS Nullable(String)), '')),
    ('$browser',     coalesce(nullIf(CAST(auto_properties['$browser']     AS Nullable(String)), ''), CAST(custom_properties['$browser']     AS Nullable(String)), '')),
    ('$device',      coalesce(nullIf(CAST(auto_properties['$device']      AS Nullable(String)), ''), CAST(custom_properties['$device']      AS Nullable(String)), '')),
    ('$platform',    coalesce(nullIf(CAST(auto_properties['$platform']    AS Nullable(String)), ''), CAST(custom_properties['$platform']    AS Nullable(String)), '')),
    ('$utmSource',   coalesce(nullIf(CAST(auto_properties['$utmSource']   AS Nullable(String)), ''), CAST(custom_properties['$utmSource']   AS Nullable(String)), '')),
    ('$utmMedium',   coalesce(nullIf(CAST(auto_properties['$utmMedium']   AS Nullable(String)), ''), CAST(custom_properties['$utmMedium']   AS Nullable(String)), '')),
    ('$utmCampaign', coalesce(nullIf(CAST(auto_properties['$utmCampaign'] AS Nullable(String)), ''), CAST(custom_properties['$utmCampaign'] AS Nullable(String)), ''))
] AS dim
GROUP BY project_id, day, kind, dim_name, dim_value;

-- +goose Down

DROP TABLE IF EXISTS dashboard_event_rollup_daily_mv;
DROP TABLE IF EXISTS dashboard_event_rollup_daily;
