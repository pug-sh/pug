package clickhouse_test

import (
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
