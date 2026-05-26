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

-- Accepted accuracy tradeoff: the ORDER BY key has no event_id, and the
-- incremental MV below sums count() at insert time. The raw events table is
-- ReplacingMergeTree keyed on (project_id, minute(occur_time), kind, event_id),
-- so it dedups retries/redeliveries on merge — but a duplicate insert is summed
-- into cnt here and can never be reconciled. So cnt (TOTAL) and the
-- cnt/uniq ratio (PER_USER_AVG) over-count vs the raw builders by the pipeline's
-- redelivery rate (monotonic, never self-correcting). uniq_state (UNIQUE_USERS)
-- is immune — uniqState on distinct_id is idempotent. This is accepted as a
-- bounded inaccuracy for dashboard visualization. If exact reconciliation with
-- the raw insights path is ever required, switch to a refreshable APPEND MV with
-- FROM events FINAL + a closed-bucket watermark (see docs/architecture/clickhouse.md).
--
-- Dimension value expressions MUST read promoted auto-property columns (not
-- auto_properties map keys) — ingest strips promoted keys into dedicated columns.
-- Keep in sync with PropertyExpr / TestMigration006PromotedDimExprsMatch.

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

-- +goose Down

DROP TABLE IF EXISTS dashboard_event_rollup_daily_mv;
DROP TABLE IF EXISTS dashboard_event_rollup_daily;
