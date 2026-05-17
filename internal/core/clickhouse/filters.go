package clickhouse

import (
	"fmt"
	"strconv"
	"strings"

	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
)

// PropertyExpr returns the ClickHouse string expression to resolve an event property.
// It reads auto_properties first and falls back to custom_properties only when
// auto's String representation is genuinely empty (i.e. the auto Variant is an
// empty String slot, OR auto is fully absent). Non-empty stringifications of
// typed auto slots (e.g. Int64 0 → '0', Bool false → 'false') BLOCK the
// fallthrough — custom is not consulted in that case. Both maps' values are
// coerced to string for unified operator handling (custom_properties stores
// Variant — CAST(v AS Nullable(String)) collapses any active variant type to
// its string representation; auto_properties uses the same Variant shape and
// cast. Numeric operators downstream re-parse via toFloat64OrNull).
//
// CAST to Nullable(String) is used (not toString) because toString(NULL Variant)
// produces the display string "ᴺᵁᴸᴸ" rather than NULL or ""; the Nullable cast
// correctly preserves NULL for absent keys so IS_SET / IS_NOT_SET work correctly
// via the existing prop != "" / prop = "" checks.
//
// Empty-value behavior is unified. Using a code block here so gofmt does not
// rewrite the paired single quotes in the prose into curly quotes:
//
//	IS_SET (prop != '') returns false in all of these:
//	  - Property absent from both maps.
//	  - Auto value is an empty String variant (nullIf collapses '' to NULL,
//	    falls through to custom). Non-string auto variants project to a
//	    non-empty string ('0', 'false', etc.) and BLOCK the fallthrough —
//	    custom is not consulted in that case.
//	  - Custom Variant String value is '' (CAST surfaces '' through the
//	    coalesce). NB: empty-string custom values are indistinguishable from
//	    absent-key here.
//
//	The trailing `, ''` sentinel in the coalesce is load-bearing: it converts
//	the fully-absent case into '' so all downstream string operators
//	(=, LIKE, IN, IS_SET) see a non-NULL projection. Removing it would break
//	IS_SET semantics.
//
// SAFETY: The name is interpolated directly into SQL (not parameterized) because ClickHouse
// map key access requires it. Callers MUST ensure name is validated before calling this function.
// At the RPC boundary, proto validation enforces the pattern ^\$?[a-zA-Z0-9_.-]+$ which
// prevents SQL injection characters. Internal callers outside the RPC chain must validate separately.
func PropertyExpr(name string) string {
	return propertyExpr(name, "")
}

func propertyExpr(name, alias string) string {
	prefix := ""
	if alias != "" {
		prefix = alias + "."
	}
	return fmt.Sprintf("coalesce(nullIf(CAST(%sauto_properties['%s'] AS Nullable(String)), ''), CAST(%scustom_properties['%s'] AS Nullable(String)), '')", prefix, name, prefix, name)
}

// propertyNumericExpr returns a Nullable(Float64) expression for numeric event
// property operators. It reads Int64/Float64 Variant slots directly and only
// falls back to string re-parsing for string-typed values, preserving the
// existing "auto first unless empty" precedence.
func propertyNumericExpr(name, alias string) string {
	prefix := ""
	if alias != "" {
		prefix = alias + "."
	}

	autoString := fmt.Sprintf("CAST(%sauto_properties['%s'] AS Nullable(String))", prefix, name)
	customString := fmt.Sprintf("CAST(%scustom_properties['%s'] AS Nullable(String))", prefix, name)
	autoNumeric := fmt.Sprintf(
		"coalesce(CAST(variantElement(%sauto_properties['%s'], 'Float64') AS Nullable(Float64)), CAST(variantElement(%sauto_properties['%s'], 'Int64') AS Nullable(Float64)), toFloat64OrNull(%s))",
		prefix, name, prefix, name, autoString,
	)
	customNumeric := fmt.Sprintf(
		"coalesce(CAST(variantElement(%scustom_properties['%s'], 'Float64') AS Nullable(Float64)), CAST(variantElement(%scustom_properties['%s'], 'Int64') AS Nullable(Float64)), toFloat64OrNull(%s))",
		prefix, name, prefix, name, customString,
	)

	return fmt.Sprintf("if(nullIf(%s, '') IS NOT NULL, %s, %s)", autoString, autoNumeric, customNumeric)
}

// ValidateProfilePropertyName rejects names that would produce malformed SQL
// when interpolated into profilePropertyPath. Mirrors the regex on
// common.v1.PropertyFilter.property at the Go layer so direct callers
// (workers, scripts) bypassing the proto interceptor get an explicit error
// instead of a CH parse failure.
func ValidateProfilePropertyName(name string) error {
	if name == "" {
		return fmt.Errorf("profile property name must not be empty")
	}
	for _, seg := range strings.Split(name, ".") {
		if seg == "" {
			return fmt.Errorf("profile property name %q must not contain empty segments", name)
		}
	}
	return nil
}

// profilePropertyPath splits `name` on `.` and joins with backtick-quoted
// segments under `properties.` (e.g. "address.city" → properties.`address`.`city`).
//
// Backticks are required because the proto pattern permits characters ($, -)
// that CH's bare-identifier parser rejects or misinterprets ('-' as subtraction).
// Bracket access (properties['k']) isn't an option — CH dispatches that to
// arrayElement, which rejects JSON-typed first arguments.
//
// SAFETY: segments are interpolated inside backtick delimiters. The proto regex
// forbids backticks in property names. Callers MUST validate `name` via
// ValidateProfilePropertyName (or the proto regex on PropertyFilter.property /
// GetPropertyValuesRequest.property_key) before calling.
func profilePropertyPath(name string) string {
	segments := strings.Split(name, ".")
	for i, s := range segments {
		segments[i] = "`" + s + "`"
	}
	return "properties." + strings.Join(segments, ".")
}

// ProfilePropertyExpr returns the string-projecting ClickHouse expression for
// a profile property. profilePropertyOperatorCondition routes operators
// between this and ProfilePropertyNumericExpr.
//
// CAST to Nullable(String) coerces typed subcolumns (Float64, Int64, Bool) to
// their string representation; the empty-string coalesce maps missing paths
// (subcolumn NULL) back to empty so IS_NOT_SET still matches absent keys.
func ProfilePropertyExpr(name string) string {
	return fmt.Sprintf("coalesce(CAST(%s AS Nullable(String)), '')", profilePropertyPath(name))
}

// ProfilePropertyNumericExpr returns the Nullable(Float64) projection of a
// profile property for numeric operators. JSON typed subcolumns (.:Float64,
// .:Int64, .:String) are strict — they return the value only if the path is
// stored as exactly that type, NULL otherwise.
//
// The coalesce ladder mirrors propertyNumericExpr's leniency for events:
// Float64 storage wins, then Int64 cast up to Float64, then a final re-parse of
// String-stored numerics (for clients that stringify numbers like
// {"ltv": "1234"}). The strict typed projections intentionally exclude Bool —
// a property stored as true/false is not meaningfully comparable to a numeric
// threshold, and CH's direct CAST(JSON AS Float64) would coerce Bool to 1/0
// with surprising filter results.
func ProfilePropertyNumericExpr(name string) string {
	path := profilePropertyPath(name)
	return fmt.Sprintf(
		"coalesce(%s.:Float64, CAST(%s.:Int64 AS Nullable(Float64)), toFloat64OrNull(%s.:String))",
		path, path, path,
	)
}

func autoPropertyMapExpr(mapExpr, name string) string {
	return fmt.Sprintf("coalesce(CAST(%s['%s'] AS Nullable(String)), '')", mapExpr, name)
}

func autoPropertyMapNumericExpr(mapExpr, name string) string {
	return fmt.Sprintf(
		"coalesce(CAST(variantElement(%s['%s'], 'Float64') AS Nullable(Float64)), CAST(variantElement(%s['%s'], 'Int64') AS Nullable(Float64)), toFloat64OrNull(CAST(%s['%s'] AS Nullable(String))))",
		mapExpr, name, mapExpr, name, mapExpr, name,
	)
}

// EscapeLike escapes ClickHouse LIKE metacharacters in a value.
func EscapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// PropertyCondition builds a typed query Condition for a PropertyFilter.
// Dispatches to profileFilterCondition when source is PROPERTY_SOURCE_PROFILE.
// For profile filters, projectID is required to build the IN subquery.
func PropertyCondition(f *commonv1.PropertyFilter, projectID string) (Condition, error) {
	return propertyCondition(f, projectID, "")
}

// PropertyConditionAliased builds a typed query Condition for a PropertyFilter,
// prefixing event column references with the given table alias.
// Dispatches to profileFilterCondition when source is PROPERTY_SOURCE_PROFILE.
func PropertyConditionAliased(f *commonv1.PropertyFilter, projectID, alias string) (Condition, error) {
	return propertyCondition(f, projectID, alias)
}

// ProfilePropertyCondition builds a Condition for profile JSON properties stored
// in the ClickHouse profiles table. Only PROFILE and UNSPECIFIED sources are
// accepted.
func ProfilePropertyCondition(f *commonv1.PropertyFilter) (Condition, error) {
	switch f.GetSource() {
	case commonv1.PropertySource_PROPERTY_SOURCE_UNSPECIFIED, commonv1.PropertySource_PROPERTY_SOURCE_PROFILE:
	default:
		return Condition{}, fmt.Errorf("unsupported profile filter source: %v", f.GetSource())
	}
	if err := ValidateProfilePropertyName(f.GetProperty()); err != nil {
		return Condition{}, err
	}
	return profilePropertyOperatorCondition(f)
}

// profilePropertyOperatorCondition routes profile property operators to typed
// expressions via routeOperator. Numeric operators read the Nullable(Float64)
// projection directly; everything else flows through the string-coalesced
// projection used for =/LIKE/IN/IS_SET semantics.
func profilePropertyOperatorCondition(f *commonv1.PropertyFilter) (Condition, error) {
	return routeOperator(f, ProfilePropertyExpr(f.GetProperty()), ProfilePropertyNumericExpr(f.GetProperty()))
}

// AutoPropertyConditionForMap builds a Condition for auto-properties already
// materialized into a Map(String, Variant(...)) column on a user/profile summary row.
func AutoPropertyConditionForMap(f *commonv1.PropertyFilter, mapExpr string) (Condition, error) {
	switch f.GetSource() {
	case commonv1.PropertySource_PROPERTY_SOURCE_UNSPECIFIED, commonv1.PropertySource_PROPERTY_SOURCE_AUTO:
	default:
		return Condition{}, fmt.Errorf("unsupported auto filter source: %v", f.GetSource())
	}
	return routeOperator(f, autoPropertyMapExpr(mapExpr, f.GetProperty()), autoPropertyMapNumericExpr(mapExpr, f.GetProperty()))
}

// AutoPropertyConditionForColumns builds a Condition for auto-properties that
// have already been materialized into dedicated string / numeric expressions.
// numericExpr may be empty for fields that have no numeric projection
// (e.g. $browser); numeric operators against such fields return an error.
func AutoPropertyConditionForColumns(f *commonv1.PropertyFilter, stringExpr, numericExpr string) (Condition, error) {
	switch f.GetSource() {
	case commonv1.PropertySource_PROPERTY_SOURCE_UNSPECIFIED, commonv1.PropertySource_PROPERTY_SOURCE_AUTO:
	default:
		return Condition{}, fmt.Errorf("unsupported auto filter source: %v", f.GetSource())
	}
	if isNumericOperator(f.GetOperator()) && numericExpr == "" {
		return Condition{}, fmt.Errorf("numeric auto filter is not supported for property %q", f.GetProperty())
	}
	return routeOperator(f, stringExpr, numericExpr)
}

func propertyCondition(f *commonv1.PropertyFilter, projectID, alias string) (Condition, error) {
	if f.GetSource() == commonv1.PropertySource_PROPERTY_SOURCE_PROFILE {
		return profileFilterCondition(projectID, f, alias)
	}
	return eventPropertyCondition(f, alias)
}

// eventPropertyCondition builds a Condition for auto/custom event property filters.
func eventPropertyCondition(f *commonv1.PropertyFilter, alias string) (Condition, error) {
	return routeOperator(f, propertyExpr(f.GetProperty(), alias), propertyNumericExpr(f.GetProperty(), alias))
}

// isNumericOperator reports whether op routes to a typed numeric projection
// (LTE/GTE/LT/GT, BETWEEN, NOT_BETWEEN). All other operators (=, !=, LIKE,
// IN, IS_SET, ...) use the string-coalesced projection.
func isNumericOperator(op commonv1.FilterOperator) bool {
	switch op {
	case commonv1.FilterOperator_FILTER_OPERATOR_LTE,
		commonv1.FilterOperator_FILTER_OPERATOR_GTE,
		commonv1.FilterOperator_FILTER_OPERATOR_LT,
		commonv1.FilterOperator_FILTER_OPERATOR_GT,
		commonv1.FilterOperator_FILTER_OPERATOR_BETWEEN,
		commonv1.FilterOperator_FILTER_OPERATOR_NOT_BETWEEN:
		return true
	}
	return false
}

// routeOperator dispatches a PropertyFilter to either the numeric (typed) or
// string (coalesced) condition builders based on the operator class. Callers
// pre-compute both projections; adding a new FilterOperator only requires
// updating isNumericOperator and this switch (or operatorCondition for string
// operators) — not every per-source caller.
func routeOperator(f *commonv1.PropertyFilter, stringExpr, numericExpr string) (Condition, error) {
	switch f.GetOperator() {
	case commonv1.FilterOperator_FILTER_OPERATOR_LTE,
		commonv1.FilterOperator_FILTER_OPERATOR_GTE,
		commonv1.FilterOperator_FILTER_OPERATOR_LT,
		commonv1.FilterOperator_FILTER_OPERATOR_GT:
		return numericCond(numericExpr, numericSQLComparator(f.GetOperator()), f, false)
	case commonv1.FilterOperator_FILTER_OPERATOR_BETWEEN:
		return betweenCond(numericExpr, f, false)
	case commonv1.FilterOperator_FILTER_OPERATOR_NOT_BETWEEN:
		return betweenCond(numericExpr, f, true)
	default:
		return operatorCondition(stringExpr, f)
	}
}

// profileFilterCondition builds a Condition that filters events by matching profile properties.
// The alias only prefixes the outer distinct_id reference; subquery columns reference
// profiles/profile_aliases tables directly and are unaffected by the alias.
//
// It generates: [alias.]distinct_id IN (
//
//	SELECT p.id FROM profiles p WHERE p.project_id=? AND p.is_deleted=0 AND p.external_id != '' AND <prop_op>
//	UNION ALL
//	SELECT pa.alias_id FROM profile_aliases pa WHERE pa.project_id=? AND pa.profile_id IN (
//	    SELECT p.id FROM profiles p WHERE p.project_id=? AND p.is_deleted=0 AND p.external_id != '' AND <prop_op>
//	)
//
// )
//
// The external_id guard excludes profiles with empty external_id, which cannot match
// any distinct_id in the events table.
func profileFilterCondition(projectID string, f *commonv1.PropertyFilter, alias string) (Condition, error) {
	if projectID == "" {
		return Condition{}, fmt.Errorf("profile property filter requires a non-empty project ID")
	}

	innerCond, err := profilePropertyOperatorCondition(f)
	if err != nil {
		return Condition{}, err
	}

	distinctIDCol := "distinct_id"
	if alias != "" {
		distinctIDCol = alias + ".distinct_id"
	}

	nonemptyCond := RawCond("p.external_id != ''")
	propertyCond := And(nonemptyCond, innerCond)

	sql := fmt.Sprintf(`%s IN (
		SELECT p.id FROM profiles p WHERE p.project_id = ? AND p.is_deleted = 0 AND %s
		UNION ALL
		SELECT pa.alias_id FROM profile_aliases pa WHERE pa.project_id = ? AND pa.profile_id IN (
			SELECT p.id FROM profiles p WHERE p.project_id = ? AND p.is_deleted = 0 AND %s
		)
	)`, distinctIDCol, propertyCond.SQL(), propertyCond.SQL())

	args := []any{projectID}
	args = append(args, propertyCond.Args()...)
	args = append(args, projectID, projectID)
	args = append(args, propertyCond.Args()...)

	if n := strings.Count(sql, "?"); n != len(args) {
		return Condition{}, fmt.Errorf("internal: filter placeholder count mismatch (%d != %d)", n, len(args))
	}
	return RawCond(sql, args...), nil
}

func numericSQLComparator(op commonv1.FilterOperator) string {
	switch op {
	case commonv1.FilterOperator_FILTER_OPERATOR_LTE:
		return "<="
	case commonv1.FilterOperator_FILTER_OPERATOR_GTE:
		return ">="
	case commonv1.FilterOperator_FILTER_OPERATOR_LT:
		return "<"
	case commonv1.FilterOperator_FILTER_OPERATOR_GT:
		return ">"
	default:
		return ""
	}
}

// numericCond parses the filter value as float64 and builds a numeric comparison.
func numericCond(prop, op string, f *commonv1.PropertyFilter, parse bool) (Condition, error) {
	n, err := strconv.ParseFloat(f.GetValue(), 64)
	if err != nil {
		return Condition{}, fmt.Errorf("invalid numeric value %q for operator %v: %w", f.GetValue(), f.GetOperator(), err)
	}
	if parse {
		prop = "toFloat64OrNull(" + prop + ")"
	}
	return RawCond(prop+" "+op+" ?", n), nil
}

func betweenCond(prop string, f *commonv1.PropertyFilter, negate bool) (Condition, error) {
	opName := "BETWEEN"
	if negate {
		opName = "NOT_BETWEEN"
	}
	if len(f.GetValues()) != 2 {
		return Condition{}, fmt.Errorf("%s operator requires exactly 2 values for property %q, got %d", opName, f.GetProperty(), len(f.GetValues()))
	}

	min, err := strconv.ParseFloat(f.GetValues()[0], 64)
	if err != nil {
		return Condition{}, fmt.Errorf("invalid numeric value %q for operator %v: %w", f.GetValues()[0], f.GetOperator(), err)
	}
	max, err := strconv.ParseFloat(f.GetValues()[1], 64)
	if err != nil {
		return Condition{}, fmt.Errorf("invalid numeric value %q for operator %v: %w", f.GetValues()[1], f.GetOperator(), err)
	}

	if negate {
		return RawCond("("+prop+" < ? OR "+prop+" > ?)", min, max), nil
	}
	return RawCond("("+prop+" >= ? AND "+prop+" <= ?)", min, max), nil
}

// operatorCondition builds a Condition for the given SQL expression and PropertyFilter operator.
//
// Upstream validation: callers via the RPC handler chain pass through `validate.NewInterceptor()`,
// which enforces the full set of PropertyFilter CEL rules (operator enum defined_only/not_in:[0],
// value_required, values_required, values_not_allowed, value_not_allowed_for_set_operators,
// numeric_value_required, between_requires_two_values, between_ordered_values).
//
// This function retains two non-validation concerns:
//   - BETWEEN/NOT_BETWEEN length check: prevents an index-out-of-range panic on direct callers
//     that bypass the interceptor before strconv.ParseFloat is called on values[0]/[1].
//   - strconv.ParseFloat: converts the string representation to float64 for the ClickHouse
//     parameter binding (toFloat64OrNull(prop) >= ?). This is a conversion, not a validation.
//
// Direct callers (workers, scripts) bypassing the interceptor must pre-validate or risk invalid SQL.
func operatorCondition(prop string, f *commonv1.PropertyFilter) (Condition, error) {
	switch f.GetOperator() {
	case commonv1.FilterOperator_FILTER_OPERATOR_EQUALS:
		return RawCond(prop+" = ?", f.GetValue()), nil
	case commonv1.FilterOperator_FILTER_OPERATOR_NOT_EQUALS:
		return RawCond(prop+" != ?", f.GetValue()), nil
	case commonv1.FilterOperator_FILTER_OPERATOR_CONTAINS:
		return RawCond(prop+" LIKE ?", "%"+EscapeLike(f.GetValue())+"%"), nil
	case commonv1.FilterOperator_FILTER_OPERATOR_NOT_CONTAINS:
		return RawCond(prop+" NOT LIKE ?", "%"+EscapeLike(f.GetValue())+"%"), nil
	case commonv1.FilterOperator_FILTER_OPERATOR_IS_SET:
		return RawCond(prop + " != ''"), nil
	case commonv1.FilterOperator_FILTER_OPERATOR_IS_NOT_SET:
		return RawCond(prop + " = ''"), nil
	case commonv1.FilterOperator_FILTER_OPERATOR_LTE:
		return numericCond(prop, "<=", f, true)
	case commonv1.FilterOperator_FILTER_OPERATOR_GTE:
		return numericCond(prop, ">=", f, true)
	case commonv1.FilterOperator_FILTER_OPERATOR_LT:
		return numericCond(prop, "<", f, true)
	case commonv1.FilterOperator_FILTER_OPERATOR_GT:
		return numericCond(prop, ">", f, true)
	case commonv1.FilterOperator_FILTER_OPERATOR_IN:
		args := make([]any, len(f.GetValues()))
		for i, v := range f.GetValues() {
			args[i] = v
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(args)), ", ")
		return RawCond(prop+" IN ("+placeholders+")", args...), nil
	case commonv1.FilterOperator_FILTER_OPERATOR_NOT_IN:
		args := make([]any, len(f.GetValues()))
		for i, v := range f.GetValues() {
			args[i] = v
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(args)), ", ")
		return RawCond(prop+" NOT IN ("+placeholders+")", args...), nil
	case commonv1.FilterOperator_FILTER_OPERATOR_BETWEEN:
		return betweenCond("toFloat64OrNull("+prop+")", f, false)
	case commonv1.FilterOperator_FILTER_OPERATOR_NOT_BETWEEN:
		return betweenCond("toFloat64OrNull("+prop+")", f, true)
	}
	return Condition{}, fmt.Errorf("unsupported filter operator: %v", f.GetOperator())
}

// EventCondition builds a typed query Condition from event filters.
// Empty input returns a zero-value Condition (no-op).
// projectID is required for profile-source property filters within event filters.
func EventCondition(events []*commonv1.EventFilter, projectID string) (Condition, error) {
	return eventCondition(events, projectID, "")
}

// EventConditionAliased builds a typed query Condition from event filters,
// prefixing event table column references (kind, auto_properties, custom_properties) with
// the given table alias. Used in JOINed CTEs where bare column names are ambiguous.
func EventConditionAliased(events []*commonv1.EventFilter, projectID, alias string) (Condition, error) {
	return eventCondition(events, projectID, alias)
}

func eventCondition(events []*commonv1.EventFilter, projectID, alias string) (Condition, error) {
	if len(events) == 0 {
		return Condition{}, nil
	}
	if len(events) == 1 {
		return singleEventCondition(events[0], projectID, -1, alias)
	}

	conds := make([]Condition, 0, len(events))
	for i, ev := range events {
		cond, err := singleEventCondition(ev, projectID, i, alias)
		if err != nil {
			return Condition{}, err
		}
		conds = append(conds, cond)
	}
	return Or(conds...), nil
}

func singleEventCondition(ev *commonv1.EventFilter, projectID string, idx int, alias string) (Condition, error) {
	if ev == nil {
		if idx >= 0 {
			return Condition{}, fmt.Errorf("event[%d]: event filter is nil", idx)
		}
		return Condition{}, fmt.Errorf("event filter is nil")
	}

	kindCol := "kind"
	if alias != "" {
		kindCol = alias + ".kind"
	}

	var conds []Condition
	if ev.GetKind() != "" {
		conds = append(conds, Eq(kindCol, ev.GetKind()))
	}
	for j, f := range ev.GetFilters() {
		cond, err := propertyCondition(f, projectID, alias)
		if err != nil {
			if idx >= 0 {
				return Condition{}, fmt.Errorf("event[%d]: filters[%d]: %w", idx, j, err)
			}
			return Condition{}, fmt.Errorf("event filter: filters[%d]: %w", j, err)
		}
		conds = append(conds, cond)
	}

	if len(conds) == 0 {
		if idx >= 0 {
			return Condition{}, fmt.Errorf("event[%d]: empty event filter in multi-event query", idx)
		}
		return Condition{}, nil
	}

	return And(conds...), nil
}
