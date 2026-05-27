-- +goose Up
-- Tile customization columns extend dashboard_tiles with per-tile presentation
-- options. Storage mirrors the existing dashboard_tiles pattern: text for proto
-- enum names (compare), jsonb for nested / repeated messages
-- (thresholds, header, visualization).
--
-- payload_hash is a sha256 of the deterministic-marshaled DashboardTileInput
-- (id cleared), maintained by the application on every write. Upsert uses it
-- to short-circuit no-op tile UPDATEs in SQL via a `where payload_hash <> $1`
-- predicate, which keeps update_time meaningful (the moddatetime trigger only
-- fires when the row is actually updated). An empty bytea default forces the
-- first write to any existing row through, since no sha256 can match it.
alter table dashboard_tiles
  add column compare       text  not null default 'COMPARE_PERIOD_UNSPECIFIED',
  add column thresholds    jsonb not null default '[]'::jsonb,
  add column header        jsonb not null default '{}'::jsonb,
  add column visualization jsonb not null default '{}'::jsonb,
  add column payload_hash  bytea not null default ''::bytea;

-- +goose Down
alter table dashboard_tiles
  drop column compare,
  drop column thresholds,
  drop column header,
  drop column visualization,
  drop column payload_hash;
