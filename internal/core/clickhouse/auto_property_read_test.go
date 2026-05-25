package clickhouse_test

import (
	"testing"

	"github.com/pug-sh/pug/internal/core/clickhouse"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	"google.golang.org/protobuf/proto"
)

func TestAutoPropertyProjectionForPromotedString(t *testing.T) {
	proj := clickhouse.AutoPropertyProjectionFor("$country", "")
	if proj.StringSQL != "coalesce(country, '')" {
		t.Fatalf("StringSQL = %q", proj.StringSQL)
	}
	if proj.NumericSQL != "toFloat64OrNull(coalesce(country, ''))" {
		t.Fatalf("NumericSQL = %q", proj.NumericSQL)
	}

	aliased := clickhouse.AutoPropertyProjectionFor("$country", "e")
	if aliased.StringSQL != "coalesce(e.country, '')" {
		t.Fatalf("aliased StringSQL = %q", aliased.StringSQL)
	}
}

func TestAutoPropertyProjectionForPromotedBool(t *testing.T) {
	proj := clickhouse.AutoPropertyProjectionFor("$mobile", "")
	if proj.StringSQL != "if(mobile, 'true', 'false')" {
		t.Fatalf("StringSQL = %q", proj.StringSQL)
	}
	if proj.NumericSQL != "" {
		t.Fatalf("NumericSQL = %q, want empty", proj.NumericSQL)
	}
}

func TestAutoPropertyProjectionForPromotedNullableUInt8(t *testing.T) {
	proj := clickhouse.AutoPropertyProjectionFor("$bot_score", "")
	if proj.StringSQL != "if(bot_score IS NOT NULL, toString(bot_score), '')" {
		t.Fatalf("StringSQL = %q", proj.StringSQL)
	}
	if proj.NumericSQL != "CAST(bot_score AS Nullable(Float64))" {
		t.Fatalf("NumericSQL = %q", proj.NumericSQL)
	}
}

func TestAutoPropertyProjectionForMapFallback(t *testing.T) {
	proj := clickhouse.AutoPropertyProjectionFor("$ip", "")
	wantString := "coalesce(nullIf(CAST(auto_properties['$ip'] AS Nullable(String)), ''), '')"
	if proj.StringSQL != wantString {
		t.Fatalf("StringSQL = %q, want %q", proj.StringSQL, wantString)
	}
	if proj.NumericSQL == "" {
		t.Fatal("expected non-empty NumericSQL for map auto property")
	}
}

func TestAutoPropertyProjectionForNonAutoKey(t *testing.T) {
	proj := clickhouse.AutoPropertyProjectionFor("email", "")
	if proj.StringSQL != "" || proj.NumericSQL != "" {
		t.Fatalf("expected zero projection for custom key, got %+v", proj)
	}
}

func TestAutoPropertyDistinctValuesForPromotedString(t *testing.T) {
	dv, ok := clickhouse.AutoPropertyDistinctValuesFor("$country")
	if !ok {
		t.Fatal("expected ok")
	}
	if dv.SelectExpr != "nullIf(country, '') AS value" {
		t.Fatalf("SelectExpr = %q", dv.SelectExpr)
	}
	if dv.NotEmptyClause != "country != ''" {
		t.Fatalf("NotEmptyClause = %q", dv.NotEmptyClause)
	}
	if len(dv.Args) != 0 {
		t.Fatalf("Args = %v, want none", dv.Args)
	}
}

func TestAutoPropertyDistinctValuesForPromotedBool(t *testing.T) {
	dv, ok := clickhouse.AutoPropertyDistinctValuesFor("$verified_bot")
	if !ok {
		t.Fatal("expected ok")
	}
	if dv.SelectExpr != "if(verified_bot, 'true', 'false') AS value" {
		t.Fatalf("SelectExpr = %q", dv.SelectExpr)
	}
	if dv.NotEmptyClause != "1" {
		t.Fatalf("NotEmptyClause = %q, want literal 1 for bool", dv.NotEmptyClause)
	}
}

func TestAutoPropertyDistinctValuesForPromotedBotScore(t *testing.T) {
	dv, ok := clickhouse.AutoPropertyDistinctValuesFor("$bot_score")
	if !ok {
		t.Fatal("expected ok")
	}
	if dv.SelectExpr != "toString(bot_score) AS value" {
		t.Fatalf("SelectExpr = %q", dv.SelectExpr)
	}
	if dv.NotEmptyClause != "bot_score IS NOT NULL" {
		t.Fatalf("NotEmptyClause = %q", dv.NotEmptyClause)
	}
}

func TestAutoPropertyDistinctValuesForMapFallback(t *testing.T) {
	dv, ok := clickhouse.AutoPropertyDistinctValuesFor("$ip")
	if !ok {
		t.Fatal("expected ok")
	}
	if dv.SelectExpr != "CAST(auto_properties[?] AS Nullable(String)) AS value" {
		t.Fatalf("SelectExpr = %q", dv.SelectExpr)
	}
	if len(dv.Args) != 1 || dv.Args[0] != "$ip" {
		t.Fatalf("Args = %v, want [$ip]", dv.Args)
	}
}

func TestAutoPropertyDistinctValuesForNonAutoKey(t *testing.T) {
	_, ok := clickhouse.AutoPropertyDistinctValuesFor("email")
	if ok {
		t.Fatal("expected ok=false for non-auto key")
	}
}

func TestPropertyExprPromotedBotScore(t *testing.T) {
	got := clickhouse.PropertyExpr("$bot_score")
	want := "if(bot_score IS NOT NULL, toString(bot_score), '')"
	if got != want {
		t.Fatalf("PropertyExpr($bot_score) = %q, want %q", got, want)
	}
}

func TestPropertyNumericExprPromotedBotScore(t *testing.T) {
	f := &commonv1.PropertyFilter{
		Property: proto.String("$bot_score"),
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_GTE.Enum(),
		Value:    proto.String("30"),
	}
	cond, err := clickhouse.PropertyCondition(f, "proj-1")
	if err != nil {
		t.Fatalf("PropertyCondition: %v", err)
	}
	want := "CAST(bot_score AS Nullable(Float64)) >= ?"
	if cond.SQL() != want {
		t.Fatalf("unexpected SQL: %s", cond.SQL())
	}
}
