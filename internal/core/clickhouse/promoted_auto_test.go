package clickhouse_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/chcol"

	"github.com/pug-sh/pug/internal/core/clickhouse"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
)

func TestSplitPromotedAutoProperties(t *testing.T) {
	src := map[string]*commonv1.PropertyValue{
		"$country":  {Value: &commonv1.PropertyValue_StringValue{StringValue: "US"}},
		"$browser":  {Value: &commonv1.PropertyValue_StringValue{StringValue: "Chrome"}},
		"$timezone": {Value: &commonv1.PropertyValue_StringValue{StringValue: "America/New_York"}},
	}
	row, rest := clickhouse.SplitPromotedAutoProperties(src)
	if row.Country != "US" || row.Browser != "Chrome" {
		t.Fatalf("unexpected promoted row: %+v", row)
	}
	if len(rest) != 1 || rest["$timezone"] == nil {
		t.Fatalf("expected only $timezone in remainder, got %#v", rest)
	}
}

func TestPromotedAutoRowMergeIntoAutoProperties(t *testing.T) {
	score := uint8(42)
	row := clickhouse.PromotedAutoRow{
		BotScore: &score,
		Country:  "US",
		Browser:  "Chrome",
		Mobile:   true,
	}
	m := row.MergeIntoAutoProperties(map[string]any{"$timezone": "Europe/Berlin"})
	if m["$country"] != "US" || m["$browser"] != "Chrome" || m["$mobile"] != "true" || m["$bot_score"] != "42" || m["$timezone"] != "Europe/Berlin" {
		t.Fatalf("unexpected merged map: %#v", m)
	}
}

func TestPromotedAutoRowMergeSkipsZeroBools(t *testing.T) {
	row := clickhouse.PromotedAutoRow{
		Country: "US",
	}
	m := row.MergeIntoAutoProperties(nil)
	if _, ok := m["$verified_bot"]; ok {
		t.Fatalf("expected $verified_bot absent for nil pointer, got %#v", m)
	}
	if _, ok := m["$mobile"]; ok {
		t.Fatalf("expected $mobile absent for zero-valued bool, got %#v", m)
	}
	if m["$country"] != "US" {
		t.Fatalf("expected $country=US, got %#v", m)
	}
}

func TestPromotedAutoRowMergeEmitsKnownVerifiedBot(t *testing.T) {
	t.Run("known false", func(t *testing.T) {
		f := false
		row := clickhouse.PromotedAutoRow{VerifiedBot: &f}
		m := row.MergeIntoAutoProperties(nil)
		if m["$verified_bot"] != "false" {
			t.Fatalf("expected $verified_bot=\"false\", got %#v", m["$verified_bot"])
		}
	})
	t.Run("known true", func(t *testing.T) {
		tr := true
		row := clickhouse.PromotedAutoRow{VerifiedBot: &tr}
		m := row.MergeIntoAutoProperties(nil)
		if m["$verified_bot"] != "true" {
			t.Fatalf("expected $verified_bot=\"true\", got %#v", m["$verified_bot"])
		}
	})
}

// TestSplitPromotedAutoPropertiesVerifiedBot exercises the proto dispatch
// path used by the events worker (processor.go SplitPromotedAutoProperties).
func TestSplitPromotedAutoPropertiesVerifiedBot(t *testing.T) {
	t.Run("known false via BoolValue", func(t *testing.T) {
		src := map[string]*commonv1.PropertyValue{
			"$verified_bot": {Value: &commonv1.PropertyValue_BoolValue{BoolValue: false}},
		}
		row, rest := clickhouse.SplitPromotedAutoProperties(src)
		if row.VerifiedBot == nil || *row.VerifiedBot != false {
			t.Fatalf("got VerifiedBot=%v, want &false", row.VerifiedBot)
		}
		if len(rest) != 0 {
			t.Fatalf("expected empty rest, got %#v", rest)
		}
	})
	t.Run("known true via BoolValue", func(t *testing.T) {
		src := map[string]*commonv1.PropertyValue{
			"$verified_bot": {Value: &commonv1.PropertyValue_BoolValue{BoolValue: true}},
		}
		row, rest := clickhouse.SplitPromotedAutoProperties(src)
		if row.VerifiedBot == nil || *row.VerifiedBot != true {
			t.Fatalf("got VerifiedBot=%v, want &true", row.VerifiedBot)
		}
		if len(rest) != 0 {
			t.Fatalf("expected empty rest, got %#v", rest)
		}
	})
	t.Run("StringValue fallback parses bool", func(t *testing.T) {
		src := map[string]*commonv1.PropertyValue{
			"$verified_bot": {Value: &commonv1.PropertyValue_StringValue{StringValue: "true"}},
		}
		row, _ := clickhouse.SplitPromotedAutoProperties(src)
		if row.VerifiedBot == nil || *row.VerifiedBot != true {
			t.Fatalf("got VerifiedBot=%v, want &true", row.VerifiedBot)
		}
	})
	t.Run("unparseable routes to rest", func(t *testing.T) {
		src := map[string]*commonv1.PropertyValue{
			"$verified_bot": {Value: &commonv1.PropertyValue_StringValue{StringValue: "maybe"}},
		}
		row, rest := clickhouse.SplitPromotedAutoProperties(src)
		if row.VerifiedBot != nil {
			t.Fatalf("expected nil VerifiedBot for unparseable input, got &%v", *row.VerifiedBot)
		}
		if rest["$verified_bot"] == nil {
			t.Fatalf("expected $verified_bot routed to rest, got %#v", rest)
		}
	})
}

// TestSplitPromotedAutoAnyPropertiesVerifiedBot exercises the map[string]any
// dispatch used by the seed and test helpers.
func TestSplitPromotedAutoAnyPropertiesVerifiedBot(t *testing.T) {
	t.Run("bool", func(t *testing.T) {
		row, rest := clickhouse.SplitPromotedAutoAnyProperties(map[string]any{"$verified_bot": true})
		if row.VerifiedBot == nil || *row.VerifiedBot != true {
			t.Fatalf("got VerifiedBot=%v, want &true", row.VerifiedBot)
		}
		if len(rest) != 0 {
			t.Fatalf("expected empty rest, got %#v", rest)
		}
	})
	t.Run("string false", func(t *testing.T) {
		row, _ := clickhouse.SplitPromotedAutoAnyProperties(map[string]any{"$verified_bot": "false"})
		if row.VerifiedBot == nil || *row.VerifiedBot != false {
			t.Fatalf("got VerifiedBot=%v, want &false", row.VerifiedBot)
		}
	})
	t.Run("non-bool non-string routes to rest", func(t *testing.T) {
		row, rest := clickhouse.SplitPromotedAutoAnyProperties(map[string]any{"$verified_bot": 42})
		if row.VerifiedBot != nil {
			t.Fatalf("expected nil VerifiedBot for rejected int, got &%v", *row.VerifiedBot)
		}
		if rest["$verified_bot"] != 42 {
			t.Fatalf("expected $verified_bot=42 routed to rest, got %#v", rest)
		}
	})
}

// TestSplitPromotedAutoVariantMapVerifiedBot exercises the chcol.Variant
// dispatch used by PrepareEventInsertArgs (test/seed insert path).
func TestSplitPromotedAutoVariantMapVerifiedBot(t *testing.T) {
	src := map[string]chcol.Variant{
		"$verified_bot": chcol.NewVariantWithType(true, "Bool"),
	}
	row, rest := clickhouse.SplitPromotedAutoVariantMap(src)
	if row.VerifiedBot == nil || *row.VerifiedBot != true {
		t.Fatalf("got VerifiedBot=%v, want &true", row.VerifiedBot)
	}
	if len(rest) != 0 {
		t.Fatalf("expected empty rest, got %#v", rest)
	}
}

// TestWebAnalyticsPromotedKeysRoundTrip pins the migration-008 promoted keys
// through the whole lockstep chain: Split extracts every key into its typed
// row field (nothing lands in the remainder), and Merge reproduces the
// canonical map keys from the row.
func TestWebAnalyticsPromotedKeysRoundTrip(t *testing.T) {
	vals := map[string]string{
		"$pathname":       "/products/ball",
		"$hostname":       "pugandpals.example.com",
		"$referrer":       "https://www.google.com/",
		"$referrerDomain": "google.com",
		"$channel":        "Organic Search",
		"$locale":         "en-US",
		"$screenSize":     "1920x1080",
		"$utmTerm":        "dog food",
		"$utmContent":     "ad-1",
		"$pageTitle":      "Tennis Ball — Pug & Pals",
	}
	src := make(map[string]*commonv1.PropertyValue, len(vals))
	for k, v := range vals {
		src[k] = &commonv1.PropertyValue{Value: &commonv1.PropertyValue_StringValue{StringValue: v}}
	}
	row, rest := clickhouse.SplitPromotedAutoProperties(src)
	if len(rest) != 0 {
		t.Fatalf("expected every web-analytics key promoted, remainder: %#v", rest)
	}
	got := map[string]string{
		"$pathname":       row.Pathname,
		"$hostname":       row.Hostname,
		"$referrer":       row.Referrer,
		"$referrerDomain": row.ReferrerDomain,
		"$channel":        row.Channel,
		"$locale":         row.Locale,
		"$screenSize":     row.ScreenSize,
		"$utmTerm":        row.UTMTerm,
		"$utmContent":     row.UTMContent,
		"$pageTitle":      row.PageTitle,
	}
	for k, want := range vals {
		if got[k] != want {
			t.Errorf("row field for %s = %q, want %q", k, got[k], want)
		}
	}
	merged := row.MergeIntoAutoProperties(nil)
	for k, want := range vals {
		if merged[k] != want {
			t.Errorf("merged[%s] = %v, want %q", k, merged[k], want)
		}
	}
}

// TestScanDestAndAppendArgsAddressTheSameFields pins the read path to the write
// path, slot by slot. Both are indexed by EventsInsertPromotedColumns, so a
// field addressed in one slot by ScanDest and emitted from another by
// AppendArgs would still round-trip self-consistently through pug — every
// promoted column is string-ish to ClickHouse, so the INSERT and the SELECT
// would both succeed while filing each value under the neighbouring column.
// Writing each column's own NAME through ScanDest and reading it back through
// AppendArgs is what makes a crossed pair visible.
//
// TestPromotedColumnOrderPinned covers the property → field → AppendArgs half
// of the chain; this covers the ScanDest half.
func TestScanDestAndAppendArgsAddressTheSameFields(t *testing.T) {
	cols := strings.Split(clickhouse.EventsInsertPromotedColumns, ", ")
	var row clickhouse.PromotedAutoRow

	dests := row.ScanDest()
	if len(dests) != len(cols) {
		t.Fatalf("ScanDest returns %d destinations, EventsInsertPromotedColumns has %d columns", len(dests), len(cols))
	}
	for i, col := range cols {
		if p, ok := dests[i].(*string); ok {
			*p = col
		}
	}

	args := row.AppendArgs()
	if len(args) != len(cols) {
		t.Fatalf("AppendArgs returns %d values, EventsInsertPromotedColumns has %d columns", len(args), len(cols))
	}
	for i, col := range cols {
		s, ok := args[i].(string)
		if !ok {
			continue // bot_score / verified_bot / mobile are not string columns
		}
		if s != col {
			t.Errorf("slot %d is column %q: wrote it via ScanDest but AppendArgs emits %q there — the two address different fields", i, col, s)
		}
	}
}

// TestPromotedStringAutoPropertiesSurfacesPickerKeys pins the keys the filter
// picker depends on (insights.mergePromotedAutoDimensions injects this list).
// $url/$referrer/$pageTitle are the load-bearing ones: they are promoted but
// deliberately not rollup dimensions, so nothing else lists them.
func TestPromotedStringAutoPropertiesSurfacesPickerKeys(t *testing.T) {
	strProps := clickhouse.PromotedStringAutoProperties()
	for _, want := range []string{"$url", "$referrer", "$pageTitle", "$pathname", "$channel"} {
		if !slices.Contains(strProps, want) {
			t.Errorf("PromotedStringAutoProperties missing %s: %v", want, strProps)
		}
	}
	for _, key := range strProps {
		if _, ok := clickhouse.PromotedColumnFor(key); !ok {
			t.Errorf("%s is surfaced to the picker but maps to no promoted column", key)
		}
	}
}
