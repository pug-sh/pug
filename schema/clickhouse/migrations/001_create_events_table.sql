-- +goose Up
CREATE TABLE IF NOT EXISTS events (
    id              String,
    project_id      String,
    distinct_id     String,
    event           String,
    sdk_properties  Map(String, String),
    user_properties Map(String, String),
    event_time      DateTime64(3),
    insert_time     DateTime64(3) DEFAULT now64(3)
) ENGINE = MergeTree()
PARTITION BY toYYYYMM(event_time)
ORDER BY (project_id, event, event_time)
SETTINGS index_granularity = 8192;

-- +goose Down
DROP TABLE IF EXISTS events;
