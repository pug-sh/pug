package purge_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/xid"

	"github.com/pug-sh/pug/internal/testutil"
)

// seedClickHouse gives the project rows in every table the purge empties. The
// event fills events and its insert-triggered rollups.
func seedClickHouse(t *testing.T, ch *testutil.TestClickHouse, projectID string, at time.Time) {
	t.Helper()
	ctx := t.Context()

	testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), projectID, "user-1", "page_view",
		uuid.NewString(), map[string]string{"$pathname": "/"}, nil, at)

	profileID := xid.New().String()
	inserts := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO profiles (id, project_id, external_id, properties, is_deleted, create_time, update_time)
		  VALUES (?, ?, ?, ?, ?, ?, ?)`,
			[]any{profileID, projectID, "user-1", `{"plan": "pro"}`, uint8(0), at, at}},
		{`INSERT INTO profile_aliases (alias_id, profile_id, external_id, project_id) VALUES (?, ?, ?, ?)`,
			[]any{"user-1", profileID, "user-1", projectID}},
		{`INSERT INTO property_keys_event_buckets
		  (project_id, map_type, kind, bucket_time, key, value_type, event_count, last_seen)
		  VALUES (?, 'auto', 'page_view', ?, '$pathname', 'String', 1, ?)`,
			[]any{projectID, at, at}},
		{`INSERT INTO property_keys_profile_current
		  (project_id, map_type, kind, key, value_type, event_count, last_seen)
		  VALUES (?, 'profile', '', 'plan', 'String', 1, ?)`,
			[]any{projectID, at}},
	}
	for _, ins := range inserts {
		if err := ch.Conn.Exec(ctx, ins.query, ins.args...); err != nil {
			t.Fatalf("seed %s: %v", ins.query, err)
		}
	}
}

// projectTables are the tables seedClickHouse fills, read straight from the
// schema rather than from the purge's own list.
func projectTables(t *testing.T, ch *testutil.TestClickHouse) []string {
	t.Helper()
	rows, err := ch.Conn.Query(t.Context(), `
		SELECT DISTINCT c.table
		FROM system.columns AS c
		INNER JOIN system.tables AS t ON t.database = c.database AND t.name = c.table
		WHERE c.database = currentDatabase() AND c.name = 'project_id'
		  AND t.engine NOT IN ('View', 'MaterializedView')
		ORDER BY c.table`)
	if err != nil {
		t.Fatalf("list project tables: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan project table: %v", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list project tables: %v", err)
	}
	return out
}

func rowCount(t *testing.T, ch *testutil.TestClickHouse, table, projectID string) uint64 {
	t.Helper()
	var n uint64
	if err := ch.Conn.QueryRow(t.Context(),
		"SELECT count() FROM "+table+" WHERE project_id = ?", projectID).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// Mutations run in the merge pool, so stopping merges holds a queued delete open.
func setMerges(t *testing.T, ch *testutil.TestClickHouse, on bool) {
	t.Helper()
	verb := "STOP"
	if on {
		verb = "START"
	}
	for _, table := range projectTables(t, ch) {
		if err := ch.Conn.Exec(t.Context(), "SYSTEM "+verb+" MERGES "+table); err != nil {
			t.Fatalf("%s merges on %s: %v", verb, table, err)
		}
	}
}

func waitForDeletes(t *testing.T, ch *testutil.TestClickHouse) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var open uint64
		if err := ch.Conn.QueryRow(t.Context(),
			"SELECT count() FROM system.mutations WHERE database = currentDatabase() AND is_done = 0").Scan(&open); err != nil {
			t.Fatalf("count open deletes: %v", err)
		}
		if open == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d deletes still open after 30s", open)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// queuedDeletes counts the deletes ever queued per table for the project.
func queuedDeletes(t *testing.T, ch *testutil.TestClickHouse, projectID string) map[string]int {
	t.Helper()
	rows, err := ch.Conn.Query(t.Context(), `
		SELECT table, count()
		FROM system.mutations
		WHERE database = currentDatabase() AND position(command, ?) > 0
		GROUP BY table`, projectID)
	if err != nil {
		t.Fatalf("list deletes: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	for rows.Next() {
		var name string
		var n uint64
		if err := rows.Scan(&name, &n); err != nil {
			t.Fatalf("scan deletes: %v", err)
		}
		out[name] = int(n)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list deletes: %v", err)
	}
	return out
}
