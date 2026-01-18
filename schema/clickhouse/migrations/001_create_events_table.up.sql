CREATE TABLE IF NOT EXISTS events (
  id String,
  user_id String,
  event_type String,
  properties String,
  timestamp DateTime DEFAULT now()
) ENGINE = MergeTree()
ORDER BY (timestamp, event_type)
SETTINGS index_granularity = 8192;
