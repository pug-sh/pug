-- +goose Up
CREATE TABLE IF NOT EXISTS events (
    id              String,
    project_id      String,
    distinct_id     String,
    kind            String,
    auto_properties   Map(String, String),
    custom_properties Map(String, String),
    occur_time      DateTime64(3),
    insert_time     DateTime64(3) DEFAULT now64(3)
) ENGINE = MergeTree()
PARTITION BY toYYYYMM(occur_time)
ORDER BY (project_id, kind, occur_time)
SETTINGS index_granularity = 8192;

-- +goose Down
DROP TABLE IF EXISTS events;
