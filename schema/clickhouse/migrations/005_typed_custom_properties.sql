-- +goose Up

-- Drop the existing events table (pre-release) and recreate with typed custom_properties.
-- auto_properties stays Map(String, String) — auto-props are server-injected and conventionally strings.
-- Drop property_keys MVs first (they were created by migration 004) — we're rebuilding
-- property_keys with an extra value_type column in the aggregate sort key.
DROP VIEW IF EXISTS property_keys_profile_mv;
DROP VIEW IF EXISTS property_keys_custom_mv;
DROP VIEW IF EXISTS property_keys_auto_mv;
DROP TABLE IF EXISTS property_keys;
DROP TABLE IF EXISTS events;

CREATE TABLE events (
    auto_properties   Map(String, String),
    custom_properties Map(String, Variant(String, Int64, Float64, Bool, DateTime64(3))),
    distinct_id       String,
    event_id          UUID,
    insert_time       DateTime64(3) DEFAULT now64(3),
    kind              LowCardinality(String),
    occur_time        DateTime64(3),
    project_id        String,
    session_id        UUID,
    INDEX idx_distinct_id distinct_id TYPE bloom_filter GRANULARITY 4,
    INDEX idx_session_id session_id TYPE bloom_filter GRANULARITY 4
) ENGINE = ReplacingMergeTree(insert_time)
PARTITION BY toYYYYMM(occur_time)
ORDER BY (project_id, toStartOfMinute(occur_time), kind, event_id)
SETTINGS index_granularity = 8192;

-- property_keys aggregates per (project, map_type, kind, key, value_type).
-- value_type is the actual variantType() of the value — exact, not inferred.
-- For auto_properties (always strings), value_type is constant 'String'.
-- For custom_properties (Variant), value_type tracks the actual Variant inner type.
-- For profile (JSON String), value_type is derived from JSON shape.
CREATE TABLE IF NOT EXISTS property_keys (
    project_id  String,
    map_type    LowCardinality(String),
    kind        LowCardinality(String),
    key         String,
    value_type  LowCardinality(String),
    event_count AggregateFunction(count),
    last_seen   AggregateFunction(max, DateTime64(3))
) ENGINE = AggregatingMergeTree()
ORDER BY (project_id, map_type, kind, key, value_type);

CREATE MATERIALIZED VIEW IF NOT EXISTS property_keys_auto_mv TO property_keys AS
SELECT
    project_id,
    'auto'              AS map_type,
    kind,
    tupleElement(kv, 1) AS key,
    'String'            AS value_type,
    countState()        AS event_count,
    maxState(occur_time) AS last_seen
FROM events
ARRAY JOIN arrayZip(mapKeys(auto_properties), mapValues(auto_properties)) AS kv
WHERE notEmpty(auto_properties)
GROUP BY project_id, map_type, kind, key, value_type;

CREATE MATERIALIZED VIEW IF NOT EXISTS property_keys_custom_mv TO property_keys AS
SELECT
    project_id,
    'custom'                          AS map_type,
    kind,
    tupleElement(kv, 1)               AS key,
    variantType(tupleElement(kv, 2))  AS value_type,
    countState()                      AS event_count,
    maxState(occur_time)              AS last_seen
FROM events
ARRAY JOIN arrayZip(mapKeys(custom_properties), mapValues(custom_properties)) AS kv
WHERE notEmpty(custom_properties)
GROUP BY project_id, map_type, kind, key, value_type;

-- Profile MV stays JSON-based — profiles are out of scope for this change.
CREATE MATERIALIZED VIEW IF NOT EXISTS property_keys_profile_mv TO property_keys AS
SELECT
    project_id,
    'profile'             AS map_type,
    ''                    AS kind,
    tupleElement(kv, 1)   AS key,
    multiIf(
        tupleElement(kv, 2) = 'null', 'None',
        startsWith(tupleElement(kv, 2), '{'), 'Object',
        startsWith(tupleElement(kv, 2), '['), 'Array',
        lowerUTF8(replaceRegexpAll(tupleElement(kv, 2), '^"|"$', '')) IN ('true','false'), 'Bool',
        startsWith(tupleElement(kv, 2), '"'), 'String',
        toFloat64OrNull(tupleElement(kv, 2)) IS NOT NULL, 'Number',
        'String'
    )                     AS value_type,
    countState()          AS event_count,
    maxState(update_time) AS last_seen
FROM profiles
ARRAY JOIN JSONExtractKeysAndValuesRaw(properties) AS kv
WHERE is_deleted = 0 AND notEmpty(properties)
GROUP BY project_id, map_type, kind, key, value_type;

-- +goose Down
DROP VIEW IF EXISTS property_keys_profile_mv;
DROP VIEW IF EXISTS property_keys_custom_mv;
DROP VIEW IF EXISTS property_keys_auto_mv;
DROP TABLE IF EXISTS property_keys;
DROP TABLE IF EXISTS events;
