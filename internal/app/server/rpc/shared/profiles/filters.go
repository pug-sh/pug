package profiles

import (
	"fmt"

	chq "github.com/pug-sh/pug/internal/core/clickhouse"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	profilesv1 "github.com/pug-sh/pug/internal/gen/proto/shared/profiles/v1"
)

func buildProfileFilterCondition(
	groups []*profilesv1.FilterGroup,
	groupsOp commonv1.LogicalOperator,
) (chq.Condition, error) {
	if len(groups) == 0 {
		return chq.Condition{}, nil
	}

	groupConds := make([]chq.Condition, 0, len(groups))
	for i, g := range groups {
		cond, err := buildSingleProfileFilterGroupCondition(g)
		if err != nil {
			return chq.Condition{}, fmt.Errorf("filter_groups[%d]: %w", i, err)
		}
		groupConds = append(groupConds, cond)
	}

	if groupsOp == commonv1.LogicalOperator_LOGICAL_OPERATOR_OR {
		return chq.Or(groupConds...), nil
	}
	return chq.And(groupConds...), nil
}

func buildSingleProfileFilterGroupCondition(group *profilesv1.FilterGroup) (chq.Condition, error) {
	if len(group.GetFilters()) == 0 {
		return chq.Condition{}, fmt.Errorf("group must contain at least one filter")
	}

	conds := make([]chq.Condition, 0, len(group.GetFilters()))
	for i, f := range group.GetFilters() {
		cond, err := buildSingleProfileFilterCondition(f)
		if err != nil {
			return chq.Condition{}, fmt.Errorf("filters[%d]: %w", i, err)
		}
		conds = append(conds, cond)
	}

	if group.GetOperator() == commonv1.LogicalOperator_LOGICAL_OPERATOR_OR {
		return chq.Or(conds...), nil
	}
	return chq.And(conds...), nil
}

func buildSingleProfileFilterCondition(f *commonv1.PropertyFilter) (chq.Condition, error) {
	switch f.GetSource() {
	case commonv1.PropertySource_PROPERTY_SOURCE_UNSPECIFIED, commonv1.PropertySource_PROPERTY_SOURCE_PROFILE:
		return chq.ProfilePropertyCondition(f)
	case commonv1.PropertySource_PROPERTY_SOURCE_AUTO:
		stringExpr, numericExpr, err := profileAutoPropertySummaryExprs(f.GetProperty())
		if err != nil {
			return chq.Condition{}, err
		}
		return chq.AutoPropertyConditionForColumns(f, stringExpr, numericExpr)
	default:
		return chq.Condition{}, fmt.Errorf("unsupported filter source: %v", f.GetSource())
	}
}

func profileAutoPropertySummaryExprs(property string) (string, string, error) {
	switch property {
	case "$browser":
		return "coalesce(activity_summary.latest_browser, '')", "", nil
	case "$browserVersion":
		return "coalesce(activity_summary.latest_browser_version, '')", "", nil
	case "$os":
		return "coalesce(activity_summary.latest_os, '')", "", nil
	case "$osVersion":
		return "coalesce(activity_summary.latest_os_version, '')", "", nil
	case "$device":
		return "coalesce(activity_summary.latest_device, '')", "", nil
	case "$country":
		return "coalesce(activity_summary.latest_country, '')", "", nil
	case "$region":
		return "coalesce(activity_summary.latest_region, '')", "", nil
	case "$city":
		return "coalesce(activity_summary.latest_city, '')", "", nil
	default:
		return "", "", fmt.Errorf("unsupported auto property %q for profile list", property)
	}
}
