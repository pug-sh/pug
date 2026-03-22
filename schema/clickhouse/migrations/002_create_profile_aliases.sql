-- +goose Up
CREATE TABLE IF NOT EXISTS profile_aliases (
    alias_id    UUID,
    profile_id  UUID,
    external_id String,
    project_id  LowCardinality(String),
    insert_time DateTime64(3) DEFAULT now64(3)
) ENGINE = ReplacingMergeTree(insert_time)
PARTITION BY toYYYYMM(insert_time)
ORDER BY (project_id, profile_id, alias_id);

-- +goose Down
DROP TABLE IF EXISTS profile_aliases;
