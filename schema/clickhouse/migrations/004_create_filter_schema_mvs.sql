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
CREATE TABLE IF NOT EXISTS property_keys (
    project_id  String,
    map_type    LowCardinality(String),
    kind        LowCardinality(String),
    key         String,
    event_count AggregateFunction(count),
    last_seen   AggregateFunction(max, DateTime64(3))
) ENGINE = AggregatingMergeTree()
ORDER BY (project_id, map_type, kind, key);

CREATE MATERIALIZED VIEW IF NOT EXISTS property_keys_auto_mv TO property_keys AS
SELECT
    project_id,
    'auto'                                  AS map_type,
    kind,
    arrayJoin(mapKeys(auto_properties))     AS key,
    countState()                            AS event_count,
    maxState(occur_time)                    AS last_seen
FROM events
WHERE notEmpty(auto_properties)
GROUP BY project_id, map_type, kind, key;

CREATE MATERIALIZED VIEW IF NOT EXISTS property_keys_custom_mv TO property_keys AS
SELECT
    project_id,
    'custom'                                AS map_type,
    kind,
    arrayJoin(mapKeys(custom_properties))   AS key,
    countState()                            AS event_count,
    maxState(occur_time)                    AS last_seen
FROM events
WHERE notEmpty(custom_properties)
GROUP BY project_id, map_type, kind, key;

CREATE MATERIALIZED VIEW IF NOT EXISTS property_keys_profile_mv TO property_keys AS
SELECT
    project_id,
    'profile'                               AS map_type,
    ''                                      AS kind,
    arrayJoin(JSONExtractKeys(properties))  AS key,
    countState()                            AS event_count,
    maxState(update_time)                   AS last_seen
FROM profiles
WHERE is_deleted = 0 AND notEmpty(properties)
GROUP BY project_id, map_type, kind, key;

-- +goose Down
DROP VIEW IF EXISTS property_keys_profile_mv;
DROP VIEW IF EXISTS property_keys_custom_mv;
DROP VIEW IF EXISTS property_keys_auto_mv;
DROP TABLE IF EXISTS property_keys;
DROP VIEW IF EXISTS event_names_mv;
DROP TABLE IF EXISTS event_names;
