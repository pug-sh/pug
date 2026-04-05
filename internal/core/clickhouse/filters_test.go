package clickhouse_test

import (
	"strings"
	"testing"

	"github.com/fivebitsio/cotton/internal/core/clickhouse"
	commonv1 "github.com/fivebitsio/cotton/internal/gen/proto/common/v1"
)

func TestPropertyExpr(t *testing.T) {
	got := clickhouse.PropertyExpr("$country")
	want := "ifNull(nullIf(auto_properties['$country'], ''), custom_properties['$country'])"
	if got != want {
		t.Errorf("PropertyExpr($country) = %q, want %q", got, want)
	}
}

func TestEscapeLike(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"hello", "hello"},
		{"100%", `100\%`},
		{"under_score", `under\_score`},
		{`back\slash`, `back\\slash`},
		{`a%b_c\d`, `a\%b\_c\\d`},
	}
	for _, tt := range tests {
		got := clickhouse.EscapeLike(tt.input)
		if got != tt.want {
			t.Errorf("EscapeLike(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestFilterClause_Equals(t *testing.T) {
	f := &commonv1.PropertyFilter{
		Property: "$country",
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS,
		Value:    "US",
	}
	clause, args, err := clickhouse.FilterClause(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if clause != "ifNull(nullIf(auto_properties['$country'], ''), custom_properties['$country']) = ?" {
		t.Errorf("unexpected clause: %s", clause)
	}
	if len(args) != 1 || args[0] != "US" {
		t.Errorf("unexpected args: %v", args)
	}
}

func TestFilterClause_Contains(t *testing.T) {
	f := &commonv1.PropertyFilter{
		Property: "name",
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_CONTAINS,
		Value:    "test%val",
	}
	clause, args, err := clickhouse.FilterClause(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if clause != "ifNull(nullIf(auto_properties['name'], ''), custom_properties['name']) LIKE ?" {
		t.Errorf("unexpected clause: %s", clause)
	}
	if len(args) != 1 || args[0] != `%test\%val%` {
		t.Errorf("unexpected args: %v (expected escaped LIKE value)", args)
	}
}

func TestFilterClause_IsSet(t *testing.T) {
	f := &commonv1.PropertyFilter{
		Property: "email",
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_IS_SET,
	}
	clause, args, err := clickhouse.FilterClause(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if clause != "ifNull(nullIf(auto_properties['email'], ''), custom_properties['email']) != ''" {
		t.Errorf("unexpected clause: %s", clause)
	}
	if len(args) != 0 {
		t.Errorf("expected no args for IS_SET, got: %v", args)
	}
}

func TestFilterClause_GTE_Numeric(t *testing.T) {
	f := &commonv1.PropertyFilter{
		Property: "score",
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_GTE,
		Value:    "42.5",
	}
	clause, args, err := clickhouse.FilterClause(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if clause != "toFloat64OrNull(ifNull(nullIf(auto_properties['score'], ''), custom_properties['score'])) >= ?" {
		t.Errorf("unexpected clause: %s", clause)
	}
	if len(args) != 1 || args[0] != 42.5 {
		t.Errorf("unexpected args: %v", args)
	}
}

func TestFilterClause_GTE_InvalidNumeric(t *testing.T) {
	f := &commonv1.PropertyFilter{
		Property: "score",
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_GTE,
		Value:    "not-a-number",
	}
	if _, _, err := clickhouse.FilterClause(f); err == nil {
		t.Fatal("expected error for non-numeric value with GTE operator")
	}
}

func TestFilterClause_In(t *testing.T) {
	f := &commonv1.PropertyFilter{
		Property: "$country",
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_IN,
		Values:   []string{"US", "UK", "CA"},
	}
	clause, args, err := clickhouse.FilterClause(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if clause != "ifNull(nullIf(auto_properties['$country'], ''), custom_properties['$country']) IN (?, ?, ?)" {
		t.Errorf("unexpected clause: %s", clause)
	}
	if len(args) != 3 {
		t.Errorf("expected 3 args, got %d: %v", len(args), args)
	}
}

func TestFilterClause_NotEquals(t *testing.T) {
	f := &commonv1.PropertyFilter{
		Property: "status",
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_NOT_EQUALS,
		Value:    "inactive",
	}
	clause, args, err := clickhouse.FilterClause(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if clause != "ifNull(nullIf(auto_properties['status'], ''), custom_properties['status']) != ?" {
		t.Errorf("unexpected clause: %s", clause)
	}
	if len(args) != 1 || args[0] != "inactive" {
		t.Errorf("unexpected args: %v", args)
	}
}

func TestFilterClause_NotContains(t *testing.T) {
	f := &commonv1.PropertyFilter{
		Property: "url",
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_NOT_CONTAINS,
		Value:    "admin",
	}
	clause, args, err := clickhouse.FilterClause(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if clause != "ifNull(nullIf(auto_properties['url'], ''), custom_properties['url']) NOT LIKE ?" {
		t.Errorf("unexpected clause: %s", clause)
	}
	if len(args) != 1 || args[0] != "%admin%" {
		t.Errorf("unexpected args: %v", args)
	}
}

func TestFilterClause_IsNotSet(t *testing.T) {
	f := &commonv1.PropertyFilter{
		Property: "email",
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_IS_NOT_SET,
	}
	clause, args, err := clickhouse.FilterClause(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if clause != "ifNull(nullIf(auto_properties['email'], ''), custom_properties['email']) = ''" {
		t.Errorf("unexpected clause: %s", clause)
	}
	if len(args) != 0 {
		t.Errorf("expected no args for IS_NOT_SET, got: %v", args)
	}
}

func TestFilterClause_LTE(t *testing.T) {
	f := &commonv1.PropertyFilter{
		Property: "age",
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_LTE,
		Value:    "30",
	}
	clause, args, err := clickhouse.FilterClause(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if clause != "toFloat64OrNull(ifNull(nullIf(auto_properties['age'], ''), custom_properties['age'])) <= ?" {
		t.Errorf("unexpected clause: %s", clause)
	}
	if len(args) != 1 || args[0] != float64(30) {
		t.Errorf("unexpected args: %v", args)
	}
}

func TestFilterClause_LT(t *testing.T) {
	f := &commonv1.PropertyFilter{
		Property: "score",
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_LT,
		Value:    "100",
	}
	clause, args, err := clickhouse.FilterClause(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if clause != "toFloat64OrNull(ifNull(nullIf(auto_properties['score'], ''), custom_properties['score'])) < ?" {
		t.Errorf("unexpected clause: %s", clause)
	}
	if len(args) != 1 || args[0] != float64(100) {
		t.Errorf("unexpected args: %v", args)
	}
}

func TestFilterClause_GT(t *testing.T) {
	f := &commonv1.PropertyFilter{
		Property: "revenue",
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_GT,
		Value:    "0",
	}
	clause, args, err := clickhouse.FilterClause(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if clause != "toFloat64OrNull(ifNull(nullIf(auto_properties['revenue'], ''), custom_properties['revenue'])) > ?" {
		t.Errorf("unexpected clause: %s", clause)
	}
	if len(args) != 1 || args[0] != float64(0) {
		t.Errorf("unexpected args: %v", args)
	}
}

func TestFilterClause_NotIn(t *testing.T) {
	f := &commonv1.PropertyFilter{
		Property: "$country",
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_NOT_IN,
		Values:   []string{"CN", "RU"},
	}
	clause, args, err := clickhouse.FilterClause(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if clause != "ifNull(nullIf(auto_properties['$country'], ''), custom_properties['$country']) NOT IN (?, ?)" {
		t.Errorf("unexpected clause: %s", clause)
	}
	if len(args) != 2 {
		t.Errorf("expected 2 args, got %d: %v", len(args), args)
	}
}

func TestFilterClause_InEmptyValues(t *testing.T) {
	f := &commonv1.PropertyFilter{
		Property: "$country",
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_IN,
		Values:   []string{},
	}
	if _, _, err := clickhouse.FilterClause(f); err == nil {
		t.Fatal("expected error for IN with empty values")
	}
}

func TestFilterClause_NotInEmptyValues(t *testing.T) {
	f := &commonv1.PropertyFilter{
		Property: "$country",
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_NOT_IN,
		Values:   []string{},
	}
	if _, _, err := clickhouse.FilterClause(f); err == nil {
		t.Fatal("expected error for NOT_IN with empty values")
	}
}

func TestFilterClause_Unsupported(t *testing.T) {
	f := &commonv1.PropertyFilter{
		Property: "x",
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_UNSPECIFIED,
	}
	if _, _, err := clickhouse.FilterClause(f); err == nil {
		t.Fatal("expected error for unsupported operator")
	}
}

func TestEventCondition_SingleKindOnly(t *testing.T) {
	cond, err := clickhouse.EventCondition([]*commonv1.EventFilter{{Kind: "page_view"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(cond.SQL(), "kind = ?") {
		t.Errorf("expected kind clause, got: %s", cond.SQL())
	}
	if len(cond.Args()) != 1 || cond.Args()[0] != "page_view" {
		t.Errorf("expected [page_view], got: %v", cond.Args())
	}
}

func TestEventCondition_MultipleEvents(t *testing.T) {
	cond, err := clickhouse.EventCondition([]*commonv1.EventFilter{
		{Kind: "page_view"},
		{Kind: "purchase"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(cond.SQL(), " OR ") {
		t.Errorf("expected OR separator, got: %s", cond.SQL())
	}
	if strings.Count(cond.SQL(), "kind = ?") != 2 {
		t.Errorf("expected 2 kind clauses, got: %s", cond.SQL())
	}
	if len(cond.Args()) != 2 {
		t.Errorf("expected 2 args, got %d: %v", len(cond.Args()), cond.Args())
	}
}

func TestEventCondition_NilEvent(t *testing.T) {
	if _, err := clickhouse.EventCondition([]*commonv1.EventFilter{nil}); err == nil {
		t.Fatal("expected error for nil EventFilter")
	} else if !strings.Contains(err.Error(), "event filter is nil") {
		t.Errorf("expected nil event filter error, got: %v", err)
	}
}

func TestEventCondition_MultipleWithEmptyFilter(t *testing.T) {
	if _, err := clickhouse.EventCondition([]*commonv1.EventFilter{
		{Kind: "page_view"},
		{},
	}); err == nil {
		t.Fatal("expected error for empty EventFilter in multi-event list")
	} else if !strings.Contains(err.Error(), "event[1]") {
		t.Errorf("expected error to include event index, got: %v", err)
	}
}

func TestEventConditionAliased(t *testing.T) {
	cond, err := clickhouse.EventConditionAliased([]*commonv1.EventFilter{
		{
			Kind: "page_view",
			Filters: []*commonv1.PropertyFilter{
				{Property: "$country", Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS, Value: "US"},
			},
		},
	}, "e")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql := cond.SQL()
	if !strings.Contains(sql, "e.kind = ?") {
		t.Errorf("expected aliased kind 'e.kind = ?', got: %s", sql)
	}
	if !strings.Contains(sql, "e.auto_properties[") {
		t.Errorf("expected aliased auto_properties 'e.auto_properties[', got: %s", sql)
	}
	if !strings.Contains(sql, "e.custom_properties[") {
		t.Errorf("expected aliased custom_properties 'e.custom_properties[', got: %s", sql)
	}
}

func TestFilterClauseAliased(t *testing.T) {
	f := &commonv1.PropertyFilter{
		Property: "$country",
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS,
		Value:    "US",
	}
	clause, args, err := clickhouse.FilterClauseAliased(f, "e")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(clause, "e.auto_properties[") {
		t.Errorf("expected aliased property, got: %s", clause)
	}
	if len(args) != 1 || args[0] != "US" {
		t.Errorf("unexpected args: %v", args)
	}
}

func TestPropertyCondition(t *testing.T) {
	f := &commonv1.PropertyFilter{
		Property: "$country",
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS,
		Value:    "US",
	}
	cond, err := clickhouse.PropertyCondition(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	clause, args, _ := clickhouse.FilterClause(f)
	if cond.SQL() != clause {
		t.Errorf("PropertyCondition SQL = %q, want %q", cond.SQL(), clause)
	}
	if len(cond.Args()) != len(args) {
		t.Errorf("PropertyCondition args len = %d, want %d", len(cond.Args()), len(args))
	}
}
