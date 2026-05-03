package clickhouse_test

import (
	"strings"
	"testing"

	"github.com/fivebitsio/cotton/internal/core/clickhouse"
	commonv1 "github.com/fivebitsio/cotton/internal/gen/proto/common/v1"
	"google.golang.org/protobuf/proto"
)

func TestPropertyExpr(t *testing.T) {
	got := clickhouse.PropertyExpr("$country")
	want := "coalesce(nullIf(CAST(auto_properties['$country'] AS Nullable(String)), ''), CAST(custom_properties['$country'] AS Nullable(String)), '')"
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

func TestPropertyCondition_Equals(t *testing.T) {
	f := &commonv1.PropertyFilter{
		Property: proto.String("$country"),
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS.Enum(),
		Value:    proto.String("US"),
	}
	cond, err := clickhouse.PropertyCondition(f, "proj1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "coalesce(nullIf(CAST(auto_properties['$country'] AS Nullable(String)), ''), CAST(custom_properties['$country'] AS Nullable(String)), '') = ?"
	if cond.SQL() != want {
		t.Errorf("unexpected SQL: %s", cond.SQL())
	}
	if len(cond.Args()) != 1 || cond.Args()[0] != "US" {
		t.Errorf("unexpected args: %v", cond.Args())
	}
}

func TestPropertyCondition_Contains(t *testing.T) {
	f := &commonv1.PropertyFilter{
		Property: proto.String("name"),
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_CONTAINS.Enum(),
		Value:    proto.String("test%val"),
	}
	cond, err := clickhouse.PropertyCondition(f, "proj1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "coalesce(nullIf(CAST(auto_properties['name'] AS Nullable(String)), ''), CAST(custom_properties['name'] AS Nullable(String)), '') LIKE ?"
	if cond.SQL() != want {
		t.Errorf("unexpected SQL: %s", cond.SQL())
	}
	if len(cond.Args()) != 1 || cond.Args()[0] != `%test\%val%` {
		t.Errorf("unexpected args: %v (expected escaped LIKE value)", cond.Args())
	}
}

func TestPropertyCondition_IsSet(t *testing.T) {
	f := &commonv1.PropertyFilter{
		Property: proto.String("email"),
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_IS_SET.Enum(),
	}
	cond, err := clickhouse.PropertyCondition(f, "proj1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "coalesce(nullIf(CAST(auto_properties['email'] AS Nullable(String)), ''), CAST(custom_properties['email'] AS Nullable(String)), '') != ''"
	if cond.SQL() != want {
		t.Errorf("unexpected SQL: %s", cond.SQL())
	}
	if len(cond.Args()) != 0 {
		t.Errorf("expected no args for IS_SET, got: %v", cond.Args())
	}
}

func TestPropertyCondition_GTE_Numeric(t *testing.T) {
	f := &commonv1.PropertyFilter{
		Property: proto.String("score"),
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_GTE.Enum(),
		Value:    proto.String("42.5"),
	}
	cond, err := clickhouse.PropertyCondition(f, "proj1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "toFloat64OrNull(coalesce(nullIf(CAST(auto_properties['score'] AS Nullable(String)), ''), CAST(custom_properties['score'] AS Nullable(String)), '')) >= ?"
	if cond.SQL() != want {
		t.Errorf("unexpected SQL: %s", cond.SQL())
	}
	if len(cond.Args()) != 1 || cond.Args()[0] != 42.5 {
		t.Errorf("unexpected args: %v", cond.Args())
	}
}

func TestPropertyCondition_GTE_InvalidNumeric(t *testing.T) {
	f := &commonv1.PropertyFilter{
		Property: proto.String("score"),
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_GTE.Enum(),
		Value:    proto.String("not-a-number"),
	}
	if _, err := clickhouse.PropertyCondition(f, "proj1"); err == nil {
		t.Fatal("expected error for non-numeric value with GTE operator")
	}
}

func TestPropertyCondition_In(t *testing.T) {
	f := &commonv1.PropertyFilter{
		Property: proto.String("$country"),
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_IN.Enum(),
		Values:   []string{"US", "UK", "CA"},
	}
	cond, err := clickhouse.PropertyCondition(f, "proj1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "coalesce(nullIf(CAST(auto_properties['$country'] AS Nullable(String)), ''), CAST(custom_properties['$country'] AS Nullable(String)), '') IN (?, ?, ?)"
	if cond.SQL() != want {
		t.Errorf("unexpected SQL: %s", cond.SQL())
	}
	if len(cond.Args()) != 3 {
		t.Errorf("expected 3 args, got %d: %v", len(cond.Args()), cond.Args())
	}
}

func TestPropertyCondition_NotEquals(t *testing.T) {
	f := &commonv1.PropertyFilter{
		Property: proto.String("status"),
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_NOT_EQUALS.Enum(),
		Value:    proto.String("inactive"),
	}
	cond, err := clickhouse.PropertyCondition(f, "proj1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "coalesce(nullIf(CAST(auto_properties['status'] AS Nullable(String)), ''), CAST(custom_properties['status'] AS Nullable(String)), '') != ?"
	if cond.SQL() != want {
		t.Errorf("unexpected SQL: %s", cond.SQL())
	}
	if len(cond.Args()) != 1 || cond.Args()[0] != "inactive" {
		t.Errorf("unexpected args: %v", cond.Args())
	}
}

func TestPropertyCondition_NotContains(t *testing.T) {
	f := &commonv1.PropertyFilter{
		Property: proto.String("url"),
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_NOT_CONTAINS.Enum(),
		Value:    proto.String("admin"),
	}
	cond, err := clickhouse.PropertyCondition(f, "proj1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "coalesce(nullIf(CAST(auto_properties['url'] AS Nullable(String)), ''), CAST(custom_properties['url'] AS Nullable(String)), '') NOT LIKE ?"
	if cond.SQL() != want {
		t.Errorf("unexpected SQL: %s", cond.SQL())
	}
	if len(cond.Args()) != 1 || cond.Args()[0] != "%admin%" {
		t.Errorf("unexpected args: %v", cond.Args())
	}
}

func TestPropertyCondition_IsNotSet(t *testing.T) {
	f := &commonv1.PropertyFilter{
		Property: proto.String("email"),
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_IS_NOT_SET.Enum(),
	}
	cond, err := clickhouse.PropertyCondition(f, "proj1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "coalesce(nullIf(CAST(auto_properties['email'] AS Nullable(String)), ''), CAST(custom_properties['email'] AS Nullable(String)), '') = ''"
	if cond.SQL() != want {
		t.Errorf("unexpected SQL: %s", cond.SQL())
	}
	if len(cond.Args()) != 0 {
		t.Errorf("expected no args for IS_NOT_SET, got: %v", cond.Args())
	}
}

func TestPropertyCondition_LTE(t *testing.T) {
	f := &commonv1.PropertyFilter{
		Property: proto.String("age"),
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_LTE.Enum(),
		Value:    proto.String("30"),
	}
	cond, err := clickhouse.PropertyCondition(f, "proj1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "toFloat64OrNull(coalesce(nullIf(CAST(auto_properties['age'] AS Nullable(String)), ''), CAST(custom_properties['age'] AS Nullable(String)), '')) <= ?"
	if cond.SQL() != want {
		t.Errorf("unexpected SQL: %s", cond.SQL())
	}
	if len(cond.Args()) != 1 || cond.Args()[0] != float64(30) {
		t.Errorf("unexpected args: %v", cond.Args())
	}
}

func TestPropertyCondition_LT(t *testing.T) {
	f := &commonv1.PropertyFilter{
		Property: proto.String("score"),
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_LT.Enum(),
		Value:    proto.String("100"),
	}
	cond, err := clickhouse.PropertyCondition(f, "proj1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "toFloat64OrNull(coalesce(nullIf(CAST(auto_properties['score'] AS Nullable(String)), ''), CAST(custom_properties['score'] AS Nullable(String)), '')) < ?"
	if cond.SQL() != want {
		t.Errorf("unexpected SQL: %s", cond.SQL())
	}
}

func TestPropertyCondition_GT(t *testing.T) {
	f := &commonv1.PropertyFilter{
		Property: proto.String("revenue"),
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_GT.Enum(),
		Value:    proto.String("0"),
	}
	cond, err := clickhouse.PropertyCondition(f, "proj1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "toFloat64OrNull(coalesce(nullIf(CAST(auto_properties['revenue'] AS Nullable(String)), ''), CAST(custom_properties['revenue'] AS Nullable(String)), '')) > ?"
	if cond.SQL() != want {
		t.Errorf("unexpected SQL: %s", cond.SQL())
	}
	if len(cond.Args()) != 1 || cond.Args()[0] != float64(0) {
		t.Errorf("unexpected args: %v", cond.Args())
	}
}

func TestPropertyCondition_NotIn(t *testing.T) {
	f := &commonv1.PropertyFilter{
		Property: proto.String("$country"),
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_NOT_IN.Enum(),
		Values:   []string{"CN", "RU"},
	}
	cond, err := clickhouse.PropertyCondition(f, "proj1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "coalesce(nullIf(CAST(auto_properties['$country'] AS Nullable(String)), ''), CAST(custom_properties['$country'] AS Nullable(String)), '') NOT IN (?, ?)"
	if cond.SQL() != want {
		t.Errorf("unexpected SQL: %s", cond.SQL())
	}
	if len(cond.Args()) != 2 {
		t.Errorf("expected 2 args, got %d: %v", len(cond.Args()), cond.Args())
	}
}

func TestPropertyCondition_Unsupported(t *testing.T) {
	f := &commonv1.PropertyFilter{
		Property: proto.String("x"),
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_UNSPECIFIED.Enum(),
	}
	if _, err := clickhouse.PropertyCondition(f, "proj1"); err == nil {
		t.Fatal("expected error for unsupported operator")
	}
}

func TestPropertyCondition_ProfileSource_Equals(t *testing.T) {
	f := &commonv1.PropertyFilter{
		Property: proto.String("plan"),
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS.Enum(),
		Value:    proto.String("pro"),
		Source:   commonv1.PropertySource_PROPERTY_SOURCE_PROFILE.Enum(),
	}
	cond, err := clickhouse.PropertyCondition(f, "proj_abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql := cond.SQL()
	if !strings.Contains(sql, "distinct_id IN (") {
		t.Errorf("expected profile subquery, got: %s", sql)
	}
	if !strings.Contains(sql, "SELECT p.id FROM profiles p WHERE") {
		t.Errorf("expected profile.id selection, got: %s", sql)
	}
	if !strings.Contains(sql, "UNION ALL") {
		t.Errorf("expected UNION ALL for aliases, got: %s", sql)
	}
	if !strings.Contains(sql, "SELECT pa.alias_id FROM profile_aliases pa") {
		t.Errorf("expected profile_aliases subquery, got: %s", sql)
	}
	if !strings.Contains(sql, "JSONExtractString(properties, 'plan')") {
		t.Errorf("expected JSONExtractString for profile property, got: %s", sql)
	}
	if !strings.Contains(sql, "is_deleted = 0") {
		t.Errorf("expected soft-delete guard, got: %s", sql)
	}
	if !strings.Contains(sql, "external_id != ''") {
		t.Errorf("expected external_id filter, got: %s", sql)
	}
	// Exact args: [projectID, filterValue, projectID, projectID, filterValue]
	// Maps to: first branch (p.project_id=?, ...plan=?), aliases (pa.project_id=?, inner p.project_id=?, ...plan=?)
	args := cond.Args()
	wantArgs := []any{"proj_abc", "pro", "proj_abc", "proj_abc", "pro"}
	if len(args) != len(wantArgs) {
		t.Fatalf("expected %d args, got %d: %v", len(wantArgs), len(args), args)
	}
	for i := range wantArgs {
		if args[i] != wantArgs[i] {
			t.Errorf("arg[%d] = %v, want %v", i, args[i], wantArgs[i])
		}
	}
}

func TestPropertyCondition_ProfileSource_IsSet(t *testing.T) {
	f := &commonv1.PropertyFilter{
		Property: proto.String("email"),
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_IS_SET.Enum(),
		Source:   commonv1.PropertySource_PROPERTY_SOURCE_PROFILE.Enum(),
	}
	cond, err := clickhouse.PropertyCondition(f, "proj_abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql := cond.SQL()
	if !strings.Contains(sql, "distinct_id IN (") {
		t.Errorf("expected profile subquery, got: %s", sql)
	}
	if !strings.Contains(sql, "JSONExtractString(properties, 'email') != ''") {
		t.Errorf("expected IS_SET condition for profile property, got: %s", sql)
	}
	// Zero-arg operator: args are only projectIDs (3 total)
	args := cond.Args()
	wantArgs := []any{"proj_abc", "proj_abc", "proj_abc"}
	if len(args) != len(wantArgs) {
		t.Fatalf("expected %d args, got %d: %v", len(wantArgs), len(args), args)
	}
	for i := range wantArgs {
		if args[i] != wantArgs[i] {
			t.Errorf("arg[%d] = %v, want %v", i, args[i], wantArgs[i])
		}
	}
}

func TestPropertyCondition_ProfileSource_IsNotSet(t *testing.T) {
	f := &commonv1.PropertyFilter{
		Property: proto.String("phone"),
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_IS_NOT_SET.Enum(),
		Source:   commonv1.PropertySource_PROPERTY_SOURCE_PROFILE.Enum(),
	}
	cond, err := clickhouse.PropertyCondition(f, "proj_abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql := cond.SQL()
	if !strings.Contains(sql, "distinct_id IN (") {
		t.Errorf("expected profile subquery, got: %s", sql)
	}
	if !strings.Contains(sql, "JSONExtractString(properties, 'phone') = ''") {
		t.Errorf("expected IS_NOT_SET condition for profile property, got: %s", sql)
	}
	// Zero-arg operator: args are only projectIDs (3 total)
	args := cond.Args()
	wantArgs := []any{"proj_abc", "proj_abc", "proj_abc"}
	if len(args) != len(wantArgs) {
		t.Fatalf("expected %d args, got %d: %v", len(wantArgs), len(args), args)
	}
	for i := range wantArgs {
		if args[i] != wantArgs[i] {
			t.Errorf("arg[%d] = %v, want %v", i, args[i], wantArgs[i])
		}
	}
}

func TestPropertyCondition_ProfileSource_In(t *testing.T) {
	f := &commonv1.PropertyFilter{
		Property: proto.String("plan"),
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_IN.Enum(),
		Values:   []string{"pro", "enterprise"},
		Source:   commonv1.PropertySource_PROPERTY_SOURCE_PROFILE.Enum(),
	}
	cond, err := clickhouse.PropertyCondition(f, "proj_abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql := cond.SQL()
	if !strings.Contains(sql, "distinct_id IN (") {
		t.Errorf("expected profile subquery, got: %s", sql)
	}
	if !strings.Contains(sql, "JSONExtractString(properties, 'plan') IN (?, ?)") {
		t.Errorf("expected IN condition for profile property, got: %s", sql)
	}
	// Multi-value operator: args are [projectID, val1, val2, projectID, projectID, val1, val2]
	args := cond.Args()
	wantArgs := []any{"proj_abc", "pro", "enterprise", "proj_abc", "proj_abc", "pro", "enterprise"}
	if len(args) != len(wantArgs) {
		t.Fatalf("expected %d args, got %d: %v", len(wantArgs), len(args), args)
	}
	for i := range wantArgs {
		if args[i] != wantArgs[i] {
			t.Errorf("arg[%d] = %v, want %v", i, args[i], wantArgs[i])
		}
	}
}

func TestPropertyCondition_ProfileSource_GTE(t *testing.T) {
	f := &commonv1.PropertyFilter{
		Property: proto.String("score"),
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_GTE.Enum(),
		Value:    proto.String("42.5"),
		Source:   commonv1.PropertySource_PROPERTY_SOURCE_PROFILE.Enum(),
	}
	cond, err := clickhouse.PropertyCondition(f, "proj_abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql := cond.SQL()
	if !strings.Contains(sql, "distinct_id IN (") {
		t.Errorf("expected profile subquery, got: %s", sql)
	}
	if !strings.Contains(sql, "toFloat64OrNull(JSONExtractString(properties, 'score')) >= ?") {
		t.Errorf("expected numeric condition for profile property, got: %s", sql)
	}
	// Numeric operator: args are [projectID, numericValue, projectID, projectID, numericValue]
	args := cond.Args()
	wantArgs := []any{"proj_abc", 42.5, "proj_abc", "proj_abc", 42.5}
	if len(args) != len(wantArgs) {
		t.Fatalf("expected %d args, got %d: %v", len(wantArgs), len(args), args)
	}
	for i := range wantArgs {
		if args[i] != wantArgs[i] {
			t.Errorf("arg[%d] = %v, want %v", i, args[i], wantArgs[i])
		}
	}
}

func TestPropertyCondition_ProfileSource_EmptyProjectID(t *testing.T) {
	f := &commonv1.PropertyFilter{
		Property: proto.String("plan"),
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS.Enum(),
		Value:    proto.String("pro"),
		Source:   commonv1.PropertySource_PROPERTY_SOURCE_PROFILE.Enum(),
	}
	_, err := clickhouse.PropertyCondition(f, "")
	if err == nil {
		t.Fatal("expected error for empty projectID with profile source")
	}
	if !strings.Contains(err.Error(), "non-empty project ID") {
		t.Errorf("expected project ID error, got: %v", err)
	}
}

func TestPropertyCondition_ProfileSource_Aliased(t *testing.T) {
	f := &commonv1.PropertyFilter{
		Property: proto.String("plan"),
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS.Enum(),
		Value:    proto.String("pro"),
		Source:   commonv1.PropertySource_PROPERTY_SOURCE_PROFILE.Enum(),
	}
	cond, err := clickhouse.PropertyConditionAliased(f, "proj_abc", "e")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql := cond.SQL()
	if !strings.Contains(sql, "e.distinct_id IN (") {
		t.Errorf("expected aliased profile subquery, got: %s", sql)
	}
	if !strings.Contains(sql, "SELECT p.id FROM profiles p WHERE") {
		t.Errorf("expected profile.id selection with alias, got: %s", sql)
	}
}

func TestPropertyConditionAliased(t *testing.T) {
	f := &commonv1.PropertyFilter{
		Property: proto.String("$country"),
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS.Enum(),
		Value:    proto.String("US"),
	}
	cond, err := clickhouse.PropertyConditionAliased(f, "proj1", "e")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(cond.SQL(), "e.auto_properties[") {
		t.Errorf("expected aliased property, got: %s", cond.SQL())
	}
	if len(cond.Args()) != 1 || cond.Args()[0] != "US" {
		t.Errorf("unexpected args: %v", cond.Args())
	}
}

func TestEventCondition_NilSlice(t *testing.T) {
	cond, err := clickhouse.EventCondition(nil, "proj1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cond.IsZero() {
		t.Errorf("expected zero Condition for nil input, got SQL: %s", cond.SQL())
	}
}

func TestEventCondition_EmptySlice(t *testing.T) {
	cond, err := clickhouse.EventCondition([]*commonv1.EventFilter{}, "proj1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cond.IsZero() {
		t.Errorf("expected zero Condition for empty input, got SQL: %s", cond.SQL())
	}
}

func TestEventCondition_SingleKindOnly(t *testing.T) {
	cond, err := clickhouse.EventCondition([]*commonv1.EventFilter{{Kind: proto.String("page_view")}}, "proj1")
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
		{Kind: proto.String("page_view")},
		{Kind: proto.String("purchase")},
	}, "proj1")
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
	if _, err := clickhouse.EventCondition([]*commonv1.EventFilter{nil}, "proj1"); err == nil {
		t.Fatal("expected error for nil EventFilter")
	} else if !strings.Contains(err.Error(), "event filter is nil") {
		t.Errorf("expected nil event filter error, got: %v", err)
	}
}

func TestEventCondition_MultipleWithEmptyFilter(t *testing.T) {
	if _, err := clickhouse.EventCondition([]*commonv1.EventFilter{
		{Kind: proto.String("page_view")},
		{},
	}, "proj1"); err == nil {
		t.Fatal("expected error for empty EventFilter in multi-event list")
	} else if !strings.Contains(err.Error(), "event[1]") {
		t.Errorf("expected error to include event index, got: %v", err)
	}
}

func TestEventConditionAliased(t *testing.T) {
	cond, err := clickhouse.EventConditionAliased([]*commonv1.EventFilter{
		{
			Kind: proto.String("page_view"),
			Filters: []*commonv1.PropertyFilter{
				{Property: proto.String("$country"), Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS.Enum(), Value: proto.String("US")},
			},
		},
	}, "proj1", "e")
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

func TestPropertyConditionAliased_NotBetween(t *testing.T) {
	f := &commonv1.PropertyFilter{
		Property: proto.String("amount"),
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_NOT_BETWEEN.Enum(),
		Values:   []string{"10", "50"},
	}
	cond, err := clickhouse.PropertyConditionAliased(f, "proj1", "e")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "(toFloat64OrNull(coalesce(nullIf(CAST(e.auto_properties['amount'] AS Nullable(String)), ''), CAST(e.custom_properties['amount'] AS Nullable(String)), '')) < ? OR toFloat64OrNull(coalesce(nullIf(CAST(e.auto_properties['amount'] AS Nullable(String)), ''), CAST(e.custom_properties['amount'] AS Nullable(String)), '')) > ?)"
	if cond.SQL() != want {
		t.Errorf("unexpected SQL:\n got: %s\nwant: %s", cond.SQL(), want)
	}
	if len(cond.Args()) != 2 {
		t.Fatalf("expected 2 args, got %d: %v", len(cond.Args()), cond.Args())
	}
	if cond.Args()[0] != float64(10) || cond.Args()[1] != float64(50) {
		t.Errorf("args = %v, want [10 50]", cond.Args())
	}
}

func TestPropertyCondition_Between(t *testing.T) {
	f := &commonv1.PropertyFilter{
		Property: proto.String("amount"),
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_BETWEEN.Enum(),
		Values:   []string{"10", "50"},
	}
	cond, err := clickhouse.PropertyCondition(f, "proj1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "(toFloat64OrNull(coalesce(nullIf(CAST(auto_properties['amount'] AS Nullable(String)), ''), CAST(custom_properties['amount'] AS Nullable(String)), '')) >= ? AND toFloat64OrNull(coalesce(nullIf(CAST(auto_properties['amount'] AS Nullable(String)), ''), CAST(custom_properties['amount'] AS Nullable(String)), '')) <= ?)"
	if cond.SQL() != want {
		t.Errorf("unexpected SQL:\n got: %s\nwant: %s", cond.SQL(), want)
	}
	if len(cond.Args()) != 2 {
		t.Fatalf("expected 2 args, got %d: %v", len(cond.Args()), cond.Args())
	}
	if cond.Args()[0] != float64(10) || cond.Args()[1] != float64(50) {
		t.Errorf("args = %v, want [10 50]", cond.Args())
	}
}

func TestPropertyCondition_NotBetween(t *testing.T) {
	f := &commonv1.PropertyFilter{
		Property: proto.String("amount"),
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_NOT_BETWEEN.Enum(),
		Values:   []string{"10", "50"},
	}
	cond, err := clickhouse.PropertyCondition(f, "proj1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "(toFloat64OrNull(coalesce(nullIf(CAST(auto_properties['amount'] AS Nullable(String)), ''), CAST(custom_properties['amount'] AS Nullable(String)), '')) < ? OR toFloat64OrNull(coalesce(nullIf(CAST(auto_properties['amount'] AS Nullable(String)), ''), CAST(custom_properties['amount'] AS Nullable(String)), '')) > ?)"
	if cond.SQL() != want {
		t.Errorf("unexpected SQL:\n got: %s\nwant: %s", cond.SQL(), want)
	}
	if len(cond.Args()) != 2 {
		t.Fatalf("expected 2 args, got %d: %v", len(cond.Args()), cond.Args())
	}
	if cond.Args()[0] != float64(10) || cond.Args()[1] != float64(50) {
		t.Errorf("args = %v, want [10 50]", cond.Args())
	}
}

func TestPropertyCondition_Between_Errors(t *testing.T) {
	tests := []struct {
		name        string
		operator    commonv1.FilterOperator
		values      []string
		wantErrFrag string
	}{
		{
			name:        "between_too_few",
			operator:    commonv1.FilterOperator_FILTER_OPERATOR_BETWEEN,
			values:      []string{"10"},
			wantErrFrag: "exactly 2 values",
		},
		{
			name:        "between_too_many",
			operator:    commonv1.FilterOperator_FILTER_OPERATOR_BETWEEN,
			values:      []string{"10", "50", "90"},
			wantErrFrag: "exactly 2 values",
		},
		{
			name:        "between_invalid_min",
			operator:    commonv1.FilterOperator_FILTER_OPERATOR_BETWEEN,
			values:      []string{"abc", "99"},
			wantErrFrag: "invalid numeric value",
		},
		{
			name:        "between_invalid_max",
			operator:    commonv1.FilterOperator_FILTER_OPERATOR_BETWEEN,
			values:      []string{"10", "xyz"},
			wantErrFrag: "invalid numeric value",
		},
		{
			name:        "not_between_too_few",
			operator:    commonv1.FilterOperator_FILTER_OPERATOR_NOT_BETWEEN,
			values:      []string{},
			wantErrFrag: "exactly 2 values",
		},
		{
			name:        "not_between_too_many",
			operator:    commonv1.FilterOperator_FILTER_OPERATOR_NOT_BETWEEN,
			values:      []string{"10", "50", "90"},
			wantErrFrag: "exactly 2 values",
		},
		{
			name:        "not_between_invalid_min",
			operator:    commonv1.FilterOperator_FILTER_OPERATOR_NOT_BETWEEN,
			values:      []string{"abc", "99"},
			wantErrFrag: "invalid numeric value",
		},
		{
			name:        "not_between_invalid_max",
			operator:    commonv1.FilterOperator_FILTER_OPERATOR_NOT_BETWEEN,
			values:      []string{"10", "xyz"},
			wantErrFrag: "invalid numeric value",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &commonv1.PropertyFilter{
				Property: proto.String("amount"),
				Operator: tt.operator.Enum(),
				Values:   tt.values,
			}
			_, err := clickhouse.PropertyCondition(f, "proj1")
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErrFrag)
			}
			if !strings.Contains(err.Error(), tt.wantErrFrag) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tt.wantErrFrag)
			}
		})
	}
}

func TestPropertyCondition_ProfileSource_Between(t *testing.T) {
	f := &commonv1.PropertyFilter{
		Property: proto.String("score"),
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_BETWEEN.Enum(),
		Values:   []string{"10", "50"},
		Source:   commonv1.PropertySource_PROPERTY_SOURCE_PROFILE.Enum(),
	}
	cond, err := clickhouse.PropertyCondition(f, "proj_abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql := cond.SQL()
	if !strings.Contains(sql, "distinct_id IN (") {
		t.Errorf("expected profile subquery, got: %s", sql)
	}
	if !strings.Contains(sql, "toFloat64OrNull(JSONExtractString(properties, 'score'))") {
		t.Errorf("expected numeric condition for profile property, got: %s", sql)
	}
	args := cond.Args()
	wantArgs := []any{"proj_abc", float64(10), float64(50), "proj_abc", "proj_abc", float64(10), float64(50)}
	if len(args) != len(wantArgs) {
		t.Fatalf("expected %d args, got %d: %v", len(wantArgs), len(args), args)
	}
	for i := range wantArgs {
		if args[i] != wantArgs[i] {
			t.Errorf("arg[%d] = %v, want %v", i, args[i], wantArgs[i])
		}
	}
}
