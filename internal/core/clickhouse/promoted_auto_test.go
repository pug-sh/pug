package clickhouse_test

import (
	"testing"

	"github.com/pug-sh/pug/internal/core/clickhouse"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
)

func TestSplitPromotedAutoProperties(t *testing.T) {
	src := map[string]*commonv1.PropertyValue{
		"$country": {Value: &commonv1.PropertyValue_StringValue{StringValue: "US"}},
		"$browser": {Value: &commonv1.PropertyValue_StringValue{StringValue: "Chrome"}},
		"$ip":      {Value: &commonv1.PropertyValue_StringValue{StringValue: "1.2.3.4"}},
	}
	row, rest := clickhouse.SplitPromotedAutoProperties(src)
	if row.Country != "US" || row.Browser != "Chrome" {
		t.Fatalf("unexpected promoted row: %+v", row)
	}
	if len(rest) != 1 || rest["$ip"] == nil {
		t.Fatalf("expected only $ip in remainder, got %#v", rest)
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
	m := row.MergeIntoAutoProperties(map[string]any{"$ip": "9.9.9.9"})
	if m["$country"] != "US" || m["$browser"] != "Chrome" || m["$mobile"] != "true" || m["$bot_score"] != "42" || m["$ip"] != "9.9.9.9" {
		t.Fatalf("unexpected merged map: %#v", m)
	}
}

func TestPromotedAutoRowMergeSkipsZeroBools(t *testing.T) {
	row := clickhouse.PromotedAutoRow{
		Country: "US",
	}
	m := row.MergeIntoAutoProperties(nil)
	if _, ok := m["$verified_bot"]; ok {
		t.Fatalf("expected $verified_bot absent for zero-valued bool, got %#v", m)
	}
	if _, ok := m["$mobile"]; ok {
		t.Fatalf("expected $mobile absent for zero-valued bool, got %#v", m)
	}
	if m["$country"] != "US" {
		t.Fatalf("expected $country=US, got %#v", m)
	}
}
