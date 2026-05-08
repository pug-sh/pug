-- +goose Up
CREATE TABLE IF NOT EXISTS events (
    auto_properties   Map(String, Variant(String, Int64, Float64, Bool, DateTime64(3))),
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

-- +goose Down
DROP TABLE IF EXISTS events;
