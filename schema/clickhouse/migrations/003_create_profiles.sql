-- +goose Up
CREATE TABLE IF NOT EXISTS profiles (
    id          String,
    project_id  LowCardinality(String),
    external_id String,
    -- max_dynamic_paths bounds per-path typed subcolumns; paths beyond the
    -- limit spill to a shared subcolumn (correct, slower to filter on).
    -- Typical profile key cardinality is well under 100; 1000 leaves headroom.
    properties  JSON(max_dynamic_paths = 1000),
    is_deleted  UInt8 DEFAULT 0,
    create_time DateTime64(3) DEFAULT toDateTime64(0, 3),
    update_time DateTime64(3) DEFAULT toDateTime64(0, 3),
    insert_time DateTime64(3) DEFAULT now64(3)
) ENGINE = ReplacingMergeTree(insert_time)
ORDER BY (project_id, id);

-- +goose Down
DROP TABLE IF EXISTS profiles;
