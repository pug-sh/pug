package retention

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/xid"

	chq "github.com/pug-sh/pug/internal/core/clickhouse"
	"github.com/pug-sh/pug/internal/testutil"
)

func TestWhereRefusesATableWithNoCut(t *testing.T) {
	if _, err := (table{name: "events"}).where([]string{"p"}, day(2026, 8, 1)); err == nil {
		t.Error("where with no cut = nil error, want a refusal rather than every row of the projects")
	}
}

// A table a migration adds and nobody cuts would keep its rows past every length.
func TestTablesCoverEveryEventTable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ch := testutil.SetupClickHouse(t)
	got := []string{"profile_aliases", "profiles", "property_keys_profile_current"}
	for _, tb := range tables {
		got = append(got, tb.name)
	}
	slices.Sort(got)
	var want []string
	if err := ch.Conn.QueryRow(t.Context(), `
		SELECT arraySort(groupUniqArray(c.table))
		FROM system.columns AS c
		INNER JOIN system.tables AS t ON t.database = c.database AND t.name = c.table
		WHERE c.database = currentDatabase() AND c.name = 'project_id'
		  AND t.engine NOT IN ('View', 'MaterializedView')`).Scan(&want); err != nil {
		t.Fatalf("list project tables: %v", err)
	}
	if !slices.Equal(got, want) {
		t.Errorf("cut and profile tables = %v, want every table with a project_id column: %v", got, want)
	}
}

// Merges stopped keep each insert its own part, so a key's partial rows sit
// apart, where a row-level check would cut the older one.
func TestWhereCutsMergedStatesByKey(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ch := testutil.SetupClickHouse(t)
	ctx := t.Context()
	for _, tb := range tables {
		if tb.key == "" {
			continue
		}
		if err := ch.Conn.Exec(ctx, "SYSTEM STOP MERGES "+tb.name); err != nil {
			t.Fatalf("stop merges on %s: %v", tb.name, err)
		}
		t.Cleanup(func() { _ = ch.Conn.Exec(context.Background(), "SYSTEM START MERGES "+tb.name) })
	}
	at := day(2026, 8, 1)
	p, gone, both := xid.New().String(), uuid.NewString(), uuid.NewString()
	for _, e := range []struct {
		name, session string
		at            time.Time
	}{
		{"gone", gone, at.AddDate(0, 0, -10)},
		{"both", both, at.AddDate(0, 0, -10)},
		{"both", both, at.AddDate(0, 0, 5)},
	} {
		testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), p, e.name, e.name, e.session, nil, nil, e.at)
	}

	want := map[string]string{"session_id": gone, "distinct_id": "gone", "kind": "gone"}
	for _, tb := range tables {
		if tb.key == "" {
			continue
		}
		where, err := tb.where([]string{p}, at)
		if err != nil {
			t.Fatalf("where on %s: %v", tb.name, err)
		}
		query, args, err := chq.NewQuery().
			Select("groupUniqArray(toString(" + tb.key + "))").
			From(tb.name).
			Where(where).
			Build()
		if err != nil {
			t.Fatalf("build on %s: %v", tb.name, err)
		}
		var got []string
		if err := ch.Conn.QueryRow(ctx, query, args...).Scan(&got); err != nil {
			t.Fatalf("%s: %v", tb.name, err)
		}
		if !slices.Equal(got, []string{want[tb.key]}) {
			t.Errorf("%s cuts %v, want only %s: a key on both sides of the cut stays whole", tb.name, got, want[tb.key])
		}
	}
}
