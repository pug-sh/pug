package clickhouse

import (
	"strings"
	"testing"

	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
)

// TestPromotedColumnOrderPinned pins the whole property → field → slot → column
// chain. TestPromotedColumnListsInLockstep compares only LENGTHS, but every
// promoted column is string-ish to ClickHouse, so swapping two of them
// type-checks, inserts without error, and silently files each value under the
// other's column — and since EventsInsertPromotedColumns drives the read path
// too, both directions rotate together and stay self-consistent-but-wrong.
//
// Each promoted string key is fed the NAME OF THE COLUMN it must reach, so
// AppendArgs slot i has to emit cols[i]. The sentinels come from
// promotedAutoColumns rather than a hand-written literal, so a newly promoted
// column is covered here without editing this test.
func TestPromotedColumnOrderPinned(t *testing.T) {
	cols := strings.Split(EventsInsertPromotedColumns, ", ")
	if len(cols) != len(promotedAutoColumns) {
		t.Fatalf("EventsInsertPromotedColumns lists %d columns, promotedAutoColumns declares %d", len(cols), len(promotedAutoColumns))
	}
	for i, c := range promotedAutoColumns {
		if c.Column != cols[i] {
			t.Errorf("promotedAutoColumns[%d] is column %q, but EventsInsertPromotedColumns slot %d is %q", i, c.Column, i, cols[i])
		}
	}

	src := make(map[string]*commonv1.PropertyValue)
	for _, c := range promotedAutoColumns {
		if c.Kind != PromotedString {
			continue
		}
		src[c.Property] = &commonv1.PropertyValue{
			Value: &commonv1.PropertyValue_StringValue{StringValue: c.Column},
		}
	}
	row, rest := SplitPromotedAutoProperties(src)
	if len(rest) != 0 {
		t.Fatalf("every promoted string key must be split out of the map, remainder: %#v", rest)
	}

	args := row.AppendArgs()
	if len(args) != len(cols) {
		t.Fatalf("AppendArgs returns %d values, want %d", len(args), len(cols))
	}
	for i, col := range cols {
		s, ok := args[i].(string)
		if !ok {
			continue // bot_score / verified_bot / mobile are not string columns
		}
		if s != col {
			t.Errorf("column %q (slot %d) receives the value belonging to column %q — the property→field→slot chain is crossed", col, i, s)
		}
	}
}

// TestPromotedStringColumnsHaveAccessors is what lets every write-path site
// derive from promotedAutoColumns instead of restating it. Str is the single
// link from a table row to its PromotedAutoRow field, so a PromotedString
// entry without one would split back out of auto_properties (the key is in
// promotedAutoByProperty) and then write nowhere — the value lost from both
// the map and the column, silently, on the ingest path.
func TestPromotedStringColumnsHaveAccessors(t *testing.T) {
	var row PromotedAutoRow
	seen := map[*string]string{}
	for _, col := range promotedAutoColumns {
		if col.Kind != PromotedString {
			if col.Str != nil {
				t.Errorf("%s is not a PromotedString column but carries a Str accessor", col.Property)
			}
			continue
		}
		if col.Str == nil {
			t.Errorf("promoted string column %q (%s) has no Str accessor", col.Column, col.Property)
			continue
		}
		// Two columns addressing one field would make each clobber the other.
		p := col.Str(&row)
		if prev, dup := seen[p]; dup {
			t.Errorf("columns %q and %q address the same PromotedAutoRow field", prev, col.Column)
		}
		seen[p] = col.Column
	}
}

// TestPromotedNonRollupKeysMapToTheirColumns pins the three promoted string
// keys that have NO rollup dimension. The rollup-backed ten are pinned to their
// columns by TestMigration011PromotedDimExprsMatch via the MV text; these three
// are not, yet mergePromotedAutoDimensions injects them into the filter picker —
// so a wrong Property→Column mapping would make every filter and breakdown on
// them read another column's data with nothing objecting.
func TestPromotedNonRollupKeysMapToTheirColumns(t *testing.T) {
	for prop, want := range map[string]string{
		"$url":       "url",
		"$referrer":  "referrer",
		"$pageTitle": "page_title",
	} {
		got, ok := promotedAutoByProperty[prop]
		if !ok {
			t.Errorf("%s is not a promoted column", prop)
			continue
		}
		if got.Column != want {
			t.Errorf("%s maps to column %q, want %q", prop, got.Column, want)
		}
	}
}
