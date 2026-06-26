package clickhouse

// ProfilesInsertStmt is the PrepareBatch INSERT for the ClickHouse profiles
// table. Shared by the profile-upsert worker and the demo seeder so the column
// list can't drift between them. Keep in sync with
// schema/clickhouse/migrations/003_create_profiles.sql.
//
// insert_time is deliberately omitted: it defaults to now64(3) on every insert
// and is the ReplacingMergeTree version column, so the latest write wins the
// merge. Adding it to this list would break that last-write-wins versioning.
const ProfilesInsertStmt = "INSERT INTO profiles (id, project_id, external_id, properties, is_deleted, create_time, update_time)"
