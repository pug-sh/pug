-- +goose Up
CREATE TABLE IF NOT EXISTS profile_aliases (
    alias_id    String,
    profile_id  String,
    external_id String,
    project_id  String,
    insert_time DateTime64(3) DEFAULT now64(3)
) ENGINE = ReplacingMergeTree(insert_time)
ORDER BY (project_id, alias_id);

-- +goose Down
DROP TABLE IF EXISTS profile_aliases;
