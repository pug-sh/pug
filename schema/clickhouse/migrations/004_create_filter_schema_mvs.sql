-- +goose Up

-- Stores per-project event name counts and last seen times, fed by events inserts.
-- Columns store partial aggregate states (AggregatingMergeTree). Query with
-- countMerge(event_count) and maxMerge(last_seen), not plain count/max.
CREATE TABLE IF NOT EXISTS event_names (
    project_id  String,
    kind        LowCardinality(String),
    event_count AggregateFunction(count),
    last_seen   AggregateFunction(max, DateTime64(3))
) ENGINE = AggregatingMergeTree()
ORDER BY (project_id, kind);

CREATE MATERIALIZED VIEW IF NOT EXISTS event_names_mv TO event_names AS
SELECT
    project_id,
    kind,
    countState()         AS event_count,
    maxState(occur_time) AS last_seen
FROM events
GROUP BY project_id, kind;

-- Stores per-project property key counts and last seen times.
-- Event-backed keys are materialized into closed occur_time buckets so refreshes
-- only scan a bounded time slice and retries stay in the same dedup bucket. Profile
-- keys are rebuilt from current-state profiles on a schedule because profile updates
-- replace whole rows and need current FINAL state to avoid stale/deleted keys.
-- Query the property_keys view with sum(event_count) and max(last_seen).
-- value_type is the actual variantType() of the value — exact, not inferred.
-- For auto_properties (always strings), value_type is constant 'String'.
-- For custom_properties (Variant), value_type tracks the actual Variant inner type.
-- For profile (JSON String), value_type is derived from JSON shape.
CREATE TABLE IF NOT EXISTS property_keys_event_buckets (
    project_id  String,
    map_type    LowCardinality(String),
    kind        LowCardinality(String),
    bucket_time DateTime64(3),
    key         String,
    value_type  LowCardinality(String),
    event_count UInt64,
    last_seen   DateTime64(3)
) ENGINE = MergeTree()
PARTITION BY toYYYYMM(bucket_time)
ORDER BY (project_id, map_type, kind, bucket_time, key, value_type);

CREATE MATERIALIZED VIEW IF NOT EXISTS property_keys_event_buckets_mv
REFRESH EVERY 5 MINUTE APPEND TO property_keys_event_buckets AS
SELECT
    project_id,
    map_type,
    kind,
    bucket_time,
    key,
    value_type,
    event_count,
    last_seen
FROM (
    SELECT
        project_id,
        'auto'              AS map_type,
        kind,
        toStartOfFiveMinutes(occur_time) AS bucket_time,
        tupleElement(kv, 1) AS key,
        'String'            AS value_type,
        count()             AS event_count,
        max(occur_time)     AS last_seen
    FROM events FINAL
    ARRAY JOIN arrayZip(mapKeys(auto_properties), mapValues(auto_properties)) AS kv
    WHERE
        notEmpty(auto_properties)
        AND toStartOfFiveMinutes(occur_time) = toStartOfFiveMinutes(now() - INTERVAL 10 MINUTE)
    GROUP BY project_id, map_type, kind, bucket_time, key, value_type

    UNION ALL

    SELECT
        project_id,
        'custom'                        AS map_type,
        kind,
        toStartOfFiveMinutes(occur_time) AS bucket_time,
        tupleElement(kv, 1)             AS key,
        variantType(tupleElement(kv, 2)) AS value_type,
        count()                         AS event_count,
        max(occur_time)                 AS last_seen
    FROM events FINAL
    ARRAY JOIN arrayZip(mapKeys(custom_properties), mapValues(custom_properties)) AS kv
    WHERE
        notEmpty(custom_properties)
        AND toStartOfFiveMinutes(occur_time) = toStartOfFiveMinutes(now() - INTERVAL 10 MINUTE)
    GROUP BY project_id, map_type, kind, bucket_time, key, value_type
)
;

CREATE TABLE IF NOT EXISTS property_keys_profile_current (
    project_id  String,
    map_type    LowCardinality(String),
    kind        LowCardinality(String),
    key         String,
    value_type  LowCardinality(String),
    event_count UInt64,
    last_seen   DateTime64(3)
) ENGINE = MergeTree()
ORDER BY (project_id, map_type, kind, key, value_type);

CREATE MATERIALIZED VIEW IF NOT EXISTS property_keys_profile_current_mv
REFRESH EVERY 5 MINUTE TO property_keys_profile_current AS
SELECT
    project_id,
    'profile'           AS map_type,
    ''                  AS kind,
    tupleElement(kv, 1) AS key,
    multiIf(
        -- JSON null literal
        tupleElement(kv, 2) = 'null', 'None',
        -- JSON object/array
        startsWith(tupleElement(kv, 2), '{'), 'Object',
        startsWith(tupleElement(kv, 2), '['), 'Array',
        -- JSON string (must come before Bool: "true"/"false" as JSON strings would
        -- otherwise be misclassified as Bool after stripping quotes).
        startsWith(tupleElement(kv, 2), '"'), 'String',
        -- JSON boolean — bare true/false (no quotes by construction at this point).
        lowerUTF8(tupleElement(kv, 2)) IN ('true', 'false'), 'Bool',
        -- JSON number — anything parseable as float that isn't a string/object/array/bool.
        toFloat64OrNull(tupleElement(kv, 2)) IS NOT NULL, 'Number',
        -- Fallback (should be unreachable in valid JSON).
        'String'
    )                   AS value_type,
    count()             AS event_count,
    max(update_time)    AS last_seen
FROM profiles FINAL
ARRAY JOIN JSONExtractKeysAndValuesRaw(properties) AS kv
WHERE is_deleted = 0 AND notEmpty(properties)
GROUP BY project_id, map_type, kind, key, value_type;

CREATE VIEW IF NOT EXISTS property_keys AS
SELECT
    project_id,
    map_type,
    kind,
    key,
    value_type,
    event_count,
    last_seen
FROM property_keys_event_buckets
UNION ALL
SELECT
    project_id,
    map_type,
    kind,
    key,
    value_type,
    event_count,
    last_seen
FROM property_keys_profile_current;

-- +goose Down
DROP VIEW IF EXISTS property_keys;
DROP VIEW IF EXISTS property_keys_profile_current_mv;
DROP TABLE IF EXISTS property_keys_profile_current;
DROP VIEW IF EXISTS property_keys_event_buckets_mv;
DROP TABLE IF EXISTS property_keys_event_buckets;
DROP VIEW IF EXISTS event_names_mv;
DROP TABLE IF EXISTS event_names;
