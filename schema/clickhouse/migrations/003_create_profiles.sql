-- +goose Up
CREATE TABLE IF NOT EXISTS profiles (
    id          String,
    project_id  LowCardinality(String),
    external_id String,
    properties  String,
    is_deleted  UInt8 DEFAULT 0,
    create_time DateTime64(3) DEFAULT toDateTime64(0, 3),
    update_time DateTime64(3) DEFAULT toDateTime64(0, 3),
    insert_time DateTime64(3) DEFAULT now64(3)
) ENGINE = ReplacingMergeTree(insert_time)
ORDER BY (project_id, id);

-- +goose Down
DROP TABLE IF EXISTS profiles;
