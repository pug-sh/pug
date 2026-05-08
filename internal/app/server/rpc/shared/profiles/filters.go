package profiles

import (
	"fmt"
	"strconv"
	"strings"

	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	profilesv1 "github.com/pug-sh/pug/internal/gen/proto/shared/profiles/v1"
)

type sqlCondition struct {
	sql  string
	args []any
}

func (c sqlCondition) isZero() bool {
	return c.sql == ""
}

func buildProfileFilterCondition(
	groups []*profilesv1.FilterGroup,
	groupsOp commonv1.LogicalOperator,
	startArg int,
) (sqlCondition, int, error) {
	if len(groups) == 0 {
		return sqlCondition{}, startArg, nil
	}

	groupConds := make([]sqlCondition, 0, len(groups))
	nextArg := startArg
	for i, g := range groups {
		cond, next, err := buildSingleFilterGroupCondition(g, nextArg)
		if err != nil {
			return sqlCondition{}, startArg, fmt.Errorf("filter_groups[%d]: %w", i, err)
		}
		groupConds = append(groupConds, cond)
		nextArg = next
	}

	if groupsOp == commonv1.LogicalOperator_LOGICAL_OPERATOR_OR {
		return joinConditions(" OR ", groupConds), nextArg, nil
	}
	return joinConditions(" AND ", groupConds), nextArg, nil
}

func buildSingleFilterGroupCondition(group *profilesv1.FilterGroup, startArg int) (sqlCondition, int, error) {
	if len(group.GetFilters()) == 0 {
		return sqlCondition{}, startArg, fmt.Errorf("group must contain at least one filter")
	}

	conds := make([]sqlCondition, 0, len(group.GetFilters()))
	nextArg := startArg
	for i, f := range group.GetFilters() {
		cond, next, err := buildPropertyFilterCondition(f, nextArg)
		if err != nil {
			return sqlCondition{}, startArg, fmt.Errorf("filters[%d]: %w", i, err)
		}
		conds = append(conds, cond)
		nextArg = next
	}

	if group.GetOperator() == commonv1.LogicalOperator_LOGICAL_OPERATOR_OR {
		return joinConditions(" OR ", conds), nextArg, nil
	}
	return joinConditions(" AND ", conds), nextArg, nil
}

func buildPropertyFilterCondition(f *commonv1.PropertyFilter, startArg int) (sqlCondition, int, error) {
	switch f.GetSource() {
	case commonv1.PropertySource_PROPERTY_SOURCE_UNSPECIFIED, commonv1.PropertySource_PROPERTY_SOURCE_PROFILE:
	default:
		return sqlCondition{}, startArg, fmt.Errorf("unsupported profile filter source: %v", f.GetSource())
	}

	propExpr := profilePropertyExpr(f.GetProperty())
	switch f.GetOperator() {
	case commonv1.FilterOperator_FILTER_OPERATOR_EQUALS:
		return withSingleArg(propExpr+" = ", startArg, f.GetValue())
	case commonv1.FilterOperator_FILTER_OPERATOR_NOT_EQUALS:
		return withSingleArg(propExpr+" != ", startArg, f.GetValue())
	case commonv1.FilterOperator_FILTER_OPERATOR_CONTAINS:
		return withSingleArg(propExpr+" LIKE ", startArg, "%"+escapeLike(f.GetValue())+"%")
	case commonv1.FilterOperator_FILTER_OPERATOR_NOT_CONTAINS:
		return withSingleArg(propExpr+" NOT LIKE ", startArg, "%"+escapeLike(f.GetValue())+"%")
	case commonv1.FilterOperator_FILTER_OPERATOR_IS_SET:
		return sqlCondition{sql: propExpr + " != ''"}, startArg, nil
	case commonv1.FilterOperator_FILTER_OPERATOR_IS_NOT_SET:
		return sqlCondition{sql: propExpr + " = ''"}, startArg, nil
	case commonv1.FilterOperator_FILTER_OPERATOR_LTE:
		return numericCond(profileNumericExpr(f.GetProperty()), "<=", f, startArg)
	case commonv1.FilterOperator_FILTER_OPERATOR_GTE:
		return numericCond(profileNumericExpr(f.GetProperty()), ">=", f, startArg)
	case commonv1.FilterOperator_FILTER_OPERATOR_LT:
		return numericCond(profileNumericExpr(f.GetProperty()), "<", f, startArg)
	case commonv1.FilterOperator_FILTER_OPERATOR_GT:
		return numericCond(profileNumericExpr(f.GetProperty()), ">", f, startArg)
	case commonv1.FilterOperator_FILTER_OPERATOR_IN:
		return inCond(propExpr, false, f, startArg)
	case commonv1.FilterOperator_FILTER_OPERATOR_NOT_IN:
		return inCond(propExpr, true, f, startArg)
	case commonv1.FilterOperator_FILTER_OPERATOR_BETWEEN:
		return betweenCond(profileNumericExpr(f.GetProperty()), false, f, startArg)
	case commonv1.FilterOperator_FILTER_OPERATOR_NOT_BETWEEN:
		return betweenCond(profileNumericExpr(f.GetProperty()), true, f, startArg)
	default:
		return sqlCondition{}, startArg, fmt.Errorf("unsupported filter operator: %v", f.GetOperator())
	}
}

func profilePropertyExpr(name string) string {
	return fmt.Sprintf("coalesce(properties->>'%s', '')", name)
}

func profileNumericExpr(name string) string {
	raw := fmt.Sprintf("properties->>'%s'", name)
	return fmt.Sprintf(
		"(case when %s ~ '^[-+]?(?:\\d+(?:\\.\\d+)?|\\.\\d+)$' then (%s)::double precision else null end)",
		raw, raw,
	)
}

func numericCond(expr, op string, f *commonv1.PropertyFilter, startArg int) (sqlCondition, int, error) {
	n, err := strconv.ParseFloat(f.GetValue(), 64)
	if err != nil {
		return sqlCondition{}, startArg, fmt.Errorf("invalid numeric value %q for operator %v: %w", f.GetValue(), f.GetOperator(), err)
	}
	return sqlCondition{
		sql:  expr + " " + op + " $" + strconv.Itoa(startArg),
		args: []any{n},
	}, startArg + 1, nil
}

func betweenCond(expr string, negate bool, f *commonv1.PropertyFilter, startArg int) (sqlCondition, int, error) {
	if len(f.GetValues()) != 2 {
		return sqlCondition{}, startArg, fmt.Errorf("%v operator requires exactly 2 values for property %q, got %d", f.GetOperator(), f.GetProperty(), len(f.GetValues()))
	}
	min, err := strconv.ParseFloat(f.GetValues()[0], 64)
	if err != nil {
		return sqlCondition{}, startArg, fmt.Errorf("invalid numeric value %q for operator %v: %w", f.GetValues()[0], f.GetOperator(), err)
	}
	max, err := strconv.ParseFloat(f.GetValues()[1], 64)
	if err != nil {
		return sqlCondition{}, startArg, fmt.Errorf("invalid numeric value %q for operator %v: %w", f.GetValues()[1], f.GetOperator(), err)
	}
	if negate {
		return sqlCondition{
			sql:  "(" + expr + " < $" + strconv.Itoa(startArg) + " OR " + expr + " > $" + strconv.Itoa(startArg+1) + ")",
			args: []any{min, max},
		}, startArg + 2, nil
	}
	return sqlCondition{
		sql:  "(" + expr + " >= $" + strconv.Itoa(startArg) + " AND " + expr + " <= $" + strconv.Itoa(startArg+1) + ")",
		args: []any{min, max},
	}, startArg + 2, nil
}

func inCond(expr string, negate bool, f *commonv1.PropertyFilter, startArg int) (sqlCondition, int, error) {
	args := make([]any, len(f.GetValues()))
	placeholders := make([]string, len(f.GetValues()))
	for i, v := range f.GetValues() {
		args[i] = v
		placeholders[i] = "$" + strconv.Itoa(startArg+i)
	}
	op := "IN"
	if negate {
		op = "NOT IN"
	}
	return sqlCondition{
		sql:  expr + " " + op + " (" + strings.Join(placeholders, ", ") + ")",
		args: args,
	}, startArg + len(args), nil
}

func withSingleArg(prefix string, startArg int, value any) (sqlCondition, int, error) {
	return sqlCondition{
		sql:  prefix + "$" + strconv.Itoa(startArg),
		args: []any{value},
	}, startArg + 1, nil
}

func joinConditions(op string, conds []sqlCondition) sqlCondition {
	active := make([]sqlCondition, 0, len(conds))
	for _, c := range conds {
		if !c.isZero() {
			active = append(active, c)
		}
	}
	switch len(active) {
	case 0:
		return sqlCondition{}
	case 1:
		return active[0]
	}
	parts := make([]string, len(active))
	args := make([]any, 0)
	for i, c := range active {
		parts[i] = c.sql
		args = append(args, c.args...)
	}
	return sqlCondition{
		sql:  "(" + strings.Join(parts, op) + ")",
		args: args,
	}
}

func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}
