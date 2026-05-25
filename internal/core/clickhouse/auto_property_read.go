package clickhouse

import (
	"fmt"
	"strings"
)

// AutoPropertyProjection is the SQL for reading one auto-property from the
// events table. StringSQL is used for =, LIKE, IN, IS_SET, breakdowns, and
// argMin; NumericSQL is Nullable(Float64) for GTE/LTE/BETWEEN and empty when
// numeric operators are unsupported (e.g. bool).
type AutoPropertyProjection struct {
	StringSQL  string
	NumericSQL string
}

// AutoPropertyProjectionFor resolves how to read key from the events table.
// key is the canonical auto-property name (e.g. "$country"). alias is an
// optional table prefix ("e" → "e.country"). Non-auto keys return zero values.
//
// Promoted keys read dedicated columns; long-tail $-prefix keys read
// auto_properties only. custom_properties is intentionally not consulted
// because proto validation forbids $ in custom property names.
func AutoPropertyProjectionFor(key, alias string) AutoPropertyProjection {
	if col, ok := promotedAutoByProperty[key]; ok {
		return AutoPropertyProjection{
			StringSQL:  promotedAutoStringSQL(col, alias),
			NumericSQL: promotedAutoNumericSQL(col, alias),
		}
	}
	if !strings.HasPrefix(key, "$") {
		return AutoPropertyProjection{}
	}
	return AutoPropertyProjection{
		StringSQL:  autoPropertyMapStringSQL(key, alias),
		NumericSQL: autoPropertyMapNumericSQL(key, alias),
	}
}

// AutoPropertyDistinctValues is the typeahead (GetPropertyValues) shape for
// auto-properties. Args holds bound parameters; non-empty only for map fallback.
type AutoPropertyDistinctValues struct {
	SelectExpr     string
	NotEmptyClause string
	Args           []any
}

// AutoPropertyDistinctValuesFor returns SELECT/WHERE fragments for distinct
// auto-property value queries. ok is false for non-auto keys.
func AutoPropertyDistinctValuesFor(key string) (AutoPropertyDistinctValues, bool) {
	if !strings.HasPrefix(key, "$") {
		return AutoPropertyDistinctValues{}, false
	}
	if col, ok := promotedAutoByProperty[key]; ok {
		return promotedAutoDistinctValues(col), true
	}
	return AutoPropertyDistinctValues{
		SelectExpr:     "CAST(auto_properties[?] AS Nullable(String)) AS value",
		NotEmptyClause: "CAST(auto_properties[?] AS Nullable(String)) != ''",
		Args:           []any{key},
	}, true
}

func tablePrefix(alias string) string {
	if alias == "" {
		return ""
	}
	return alias + "."
}

func promotedAutoStringSQL(col PromotedAutoColumn, alias string) string {
	prefix := tablePrefix(alias)
	switch col.Kind {
	case PromotedString:
		return fmt.Sprintf("coalesce(%s%s, '')", prefix, col.Column)
	case PromotedBool:
		return fmt.Sprintf("if(%s%s, 'true', 'false')", prefix, col.Column)
	case PromotedNullableUInt8:
		return fmt.Sprintf("if(%s%s IS NOT NULL, toString(%s%s), '')", prefix, col.Column, prefix, col.Column)
	default:
		return ""
	}
}

func promotedAutoNumericSQL(col PromotedAutoColumn, alias string) string {
	prefix := tablePrefix(alias)
	switch col.Kind {
	case PromotedNullableUInt8:
		return fmt.Sprintf("CAST(%s%s AS Nullable(Float64))", prefix, col.Column)
	case PromotedString:
		return fmt.Sprintf("toFloat64OrNull(%s)", promotedAutoStringSQL(col, alias))
	default:
		return ""
	}
}

// autoPropertyMapStringSQL reads a non-promoted $-prefix key from
// auto_properties only. custom_properties is not consulted because proto
// validation forbids $ in custom property names.
func autoPropertyMapStringSQL(key, alias string) string {
	prefix := tablePrefix(alias)
	return fmt.Sprintf("coalesce(nullIf(CAST(%sauto_properties['%s'] AS Nullable(String)), ''), '')", prefix, key)
}

// autoPropertyMapNumericSQL returns a Nullable(Float64) for a non-promoted
// $-prefix key. Falls back to NULL (not custom_properties) because proto
// validation forbids $ in custom property names.
func autoPropertyMapNumericSQL(key, alias string) string {
	prefix := tablePrefix(alias)
	autoString := fmt.Sprintf("CAST(%sauto_properties['%s'] AS Nullable(String))", prefix, key)
	autoNumeric := fmt.Sprintf(
		"coalesce(CAST(variantElement(%sauto_properties['%s'], 'Float64') AS Nullable(Float64)), CAST(variantElement(%sauto_properties['%s'], 'Int64') AS Nullable(Float64)), toFloat64OrNull(%s))",
		prefix, key, prefix, key, autoString,
	)
	return fmt.Sprintf("if(nullIf(%s, '') IS NOT NULL, %s, CAST(NULL AS Nullable(Float64)))", autoString, autoNumeric)
}

func promotedAutoDistinctValues(col PromotedAutoColumn) AutoPropertyDistinctValues {
	switch col.Kind {
	case PromotedString:
		return AutoPropertyDistinctValues{
			SelectExpr:     fmt.Sprintf("nullIf(%s, '') AS value", col.Column),
			NotEmptyClause: fmt.Sprintf("%s != ''", col.Column),
		}
	case PromotedBool:
		return AutoPropertyDistinctValues{
			SelectExpr:     fmt.Sprintf("if(%s, 'true', 'false') AS value", col.Column),
			NotEmptyClause: "1",
		}
	case PromotedNullableUInt8:
		return AutoPropertyDistinctValues{
			SelectExpr:     fmt.Sprintf("toString(%s) AS value", col.Column),
			NotEmptyClause: fmt.Sprintf("%s IS NOT NULL", col.Column),
		}
	default:
		return AutoPropertyDistinctValues{}
	}
}
