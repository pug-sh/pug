-- +goose Up
CREATE TABLE IF NOT EXISTS events (
    auto_properties   Map(String, String),
    custom_properties Map(String, String),
    distinct_id       String,
    id                String,
    insert_time       DateTime64(3) DEFAULT now64(3),
    kind              String,
    occur_time        DateTime64(3),
    project_id        String
) ENGINE = MergeTree()
PARTITION BY toYYYYMM(occur_time)
ORDER BY (project_id, kind, occur_time)
SETTINGS index_granularity = 8192;

-- +goose Down
DROP TABLE IF EXISTS events;
