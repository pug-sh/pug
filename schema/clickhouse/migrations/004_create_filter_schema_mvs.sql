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

-- Stores per-project property key counts and last seen times, fed by events inserts.
-- Columns store partial aggregate states (AggregatingMergeTree). Query with
-- countMerge(event_count) and maxMerge(last_seen), not plain count/max.
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
    'auto'                          AS map_type,
    kind,
    tupleElement(kv, 1)             AS key,
    'String'                        AS value_type,
    countState()                    AS event_count,
    maxState(occur_time)            AS last_seen
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

CREATE MATERIALIZED VIEW IF NOT EXISTS property_keys_profile_mv TO property_keys AS
SELECT
    project_id,
    'profile'             AS map_type,
    ''                    AS kind,
    tupleElement(kv, 1)   AS key,
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
DROP VIEW IF EXISTS event_names_mv;
DROP TABLE IF EXISTS event_names;
