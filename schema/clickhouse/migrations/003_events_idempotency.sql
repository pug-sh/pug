-- +goose Up
ALTER TABLE events RENAME COLUMN id TO event_id;
ALTER TABLE events MODIFY ORDER BY (project_id, event_id);
ALTER TABLE events MODIFY ENGINE ReplacingMergeTree();

-- +goose Down
ALTER TABLE events MODIFY ENGINE MergeTree();
ALTER TABLE events MODIFY ORDER BY (project_id, kind, occur_time);
ALTER TABLE events RENAME COLUMN event_id TO id;
