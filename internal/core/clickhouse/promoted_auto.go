package clickhouse

import (
	"fmt"

	"github.com/ClickHouse/clickhouse-go/v2/lib/chcol"

	"github.com/pug-sh/pug/internal/attribution"
	"github.com/pug-sh/pug/internal/autoprop"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	"github.com/pug-sh/pug/internal/geo"
	"github.com/pug-sh/pug/internal/useragent"
)

// PromotedAutoColumnKind describes how a promoted auto-property is stored on
// the events table and how PropertyExpr projects it to string/numeric SQL.
type PromotedAutoColumnKind int

const (
	PromotedString PromotedAutoColumnKind = iota
	PromotedBool
	PromotedNullableBool
	PromotedNullableUInt8
)

// PromotedAutoColumn maps a well-known auto-property key to a dedicated events
// table column. Order matches EventsInsertPromotedColumns and PromotedAutoRow.
//
// Str addresses the row field backing a PromotedString column, and is what
// makes this table authoritative for the write path rather than merely
// parallel to it: splitting, merging back, and the filter picker all derive
// from it, so a new promoted string column is one row here plus one struct
// field — not a case in each of several hand-written switches, where the one
// that gets forgotten decides which surface silently loses the value. Nil for
// the non-string kinds, which have no single string field to address.
type PromotedAutoColumn struct {
	Property string
	Column   string
	Kind     PromotedAutoColumnKind
	Str      func(*PromotedAutoRow) *string
}

// promotedAutoColumns is the authoritative list of auto-properties extracted
// from auto_properties at ingest. Keep in sync with
// schema/clickhouse/migrations/001_create_events_table.sql and
// 008_add_web_analytics_columns.sql.
var promotedAutoColumns = []PromotedAutoColumn{
	{Property: autoprop.PropBotScore, Column: "bot_score", Kind: PromotedNullableUInt8},
	{Property: autoprop.PropVerifiedBot, Column: "verified_bot", Kind: PromotedNullableBool},
	{Property: autoprop.PropMobile, Column: "mobile", Kind: PromotedBool},
	{Property: geo.PropCountry, Column: "country", Kind: PromotedString, Str: func(r *PromotedAutoRow) *string { return &r.Country }},
	{Property: geo.PropRegion, Column: "region", Kind: PromotedString, Str: func(r *PromotedAutoRow) *string { return &r.Region }},
	{Property: geo.PropCity, Column: "city", Kind: PromotedString, Str: func(r *PromotedAutoRow) *string { return &r.City }},
	{Property: useragent.PropBrowser, Column: "browser", Kind: PromotedString, Str: func(r *PromotedAutoRow) *string { return &r.Browser }},
	{Property: useragent.PropBrowserVersion, Column: "browser_version", Kind: PromotedString, Str: func(r *PromotedAutoRow) *string { return &r.BrowserVersion }},
	{Property: useragent.PropOS, Column: "os", Kind: PromotedString, Str: func(r *PromotedAutoRow) *string { return &r.OS }},
	{Property: useragent.PropOSVersion, Column: "os_version", Kind: PromotedString, Str: func(r *PromotedAutoRow) *string { return &r.OSVersion }},
	{Property: useragent.PropDevice, Column: "device", Kind: PromotedString, Str: func(r *PromotedAutoRow) *string { return &r.Device }},
	{Property: "$platform", Column: "platform", Kind: PromotedString, Str: func(r *PromotedAutoRow) *string { return &r.Platform }},
	{Property: attribution.PropURL, Column: "url", Kind: PromotedString, Str: func(r *PromotedAutoRow) *string { return &r.URL }},
	{Property: attribution.PropUTMSource, Column: "utm_source", Kind: PromotedString, Str: func(r *PromotedAutoRow) *string { return &r.UTMSource }},
	{Property: attribution.PropUTMMedium, Column: "utm_medium", Kind: PromotedString, Str: func(r *PromotedAutoRow) *string { return &r.UTMMedium }},
	{Property: attribution.PropUTMCampaign, Column: "utm_campaign", Kind: PromotedString, Str: func(r *PromotedAutoRow) *string { return &r.UTMCampaign }},
	{Property: attribution.PropPathname, Column: "pathname", Kind: PromotedString, Str: func(r *PromotedAutoRow) *string { return &r.Pathname }},
	{Property: attribution.PropHostname, Column: "hostname", Kind: PromotedString, Str: func(r *PromotedAutoRow) *string { return &r.Hostname }},
	{Property: attribution.PropReferrer, Column: "referrer", Kind: PromotedString, Str: func(r *PromotedAutoRow) *string { return &r.Referrer }},
	{Property: attribution.PropReferrerDomain, Column: "referrer_domain", Kind: PromotedString, Str: func(r *PromotedAutoRow) *string { return &r.ReferrerDomain }},
	{Property: attribution.PropChannel, Column: "channel", Kind: PromotedString, Str: func(r *PromotedAutoRow) *string { return &r.Channel }},
	{Property: attribution.PropLocale, Column: "locale", Kind: PromotedString, Str: func(r *PromotedAutoRow) *string { return &r.Locale }},
	{Property: attribution.PropScreenSize, Column: "screen_size", Kind: PromotedString, Str: func(r *PromotedAutoRow) *string { return &r.ScreenSize }},
	{Property: attribution.PropUTMTerm, Column: "utm_term", Kind: PromotedString, Str: func(r *PromotedAutoRow) *string { return &r.UTMTerm }},
	{Property: attribution.PropUTMContent, Column: "utm_content", Kind: PromotedString, Str: func(r *PromotedAutoRow) *string { return &r.UTMContent }},
	{Property: attribution.PropPageTitle, Column: "page_title", Kind: PromotedString, Str: func(r *PromotedAutoRow) *string { return &r.PageTitle }},
}

var promotedAutoByProperty map[string]PromotedAutoColumn

func init() {
	promotedAutoByProperty = make(map[string]PromotedAutoColumn, len(promotedAutoColumns))
	for _, col := range promotedAutoColumns {
		promotedAutoByProperty[col.Property] = col
	}
}

// PromotedColumnFor returns the events column an auto-property is promoted
// into. promotedAutoColumns is the authoritative property↔column mapping, so
// callers that need a column name derive it here rather than restating the
// pairs — the rollup migrations name their per-dimension artifacts after this
// column (dashboard_session_rollup's entry_<column>_state), and a second copy
// of the mapping is a drift source with no compile-time link back.
func PromotedColumnFor(property string) (string, bool) {
	col, ok := promotedAutoByProperty[property]
	if !ok {
		return "", false
	}
	return col.Column, true
}

// PromotedStringAutoProperties returns the canonical property keys of every
// PromotedString events column, in declaration order. Ingest strips these
// keys from the auto_properties map, so the property_keys discovery MV never
// observes them — the filter-schema picker injects this list to keep them
// discoverable (insights.mergePromotedAutoDimensions).
func PromotedStringAutoProperties() []string {
	out := make([]string, 0, len(promotedAutoColumns))
	for _, col := range promotedAutoColumns {
		if col.Kind == PromotedString {
			out = append(out, col.Property)
		}
	}
	return out
}

// EventsInsertPromotedColumns lists promoted auto-property columns on the events
// table, in PromotedAutoRow.AppendArgs / ScanDest order.
const EventsInsertPromotedColumns = `bot_score, verified_bot, mobile, country, region, city, browser, browser_version, os, os_version, device, platform, url, utm_source, utm_medium, utm_campaign, pathname, hostname, referrer, referrer_domain, channel, locale, screen_size, utm_term, utm_content, page_title`

// EventsInsertColumns is the full INSERT column list for the events table.
// insert_time is omitted; ClickHouse fills it via DEFAULT now64(3).
const EventsInsertColumns = `event_id, project_id, distinct_id, kind, auto_properties, custom_properties, ` + EventsInsertPromotedColumns + `, occur_time, session_id`

// EventsInsertStmt is the PrepareBatch INSERT for the events worker, seed, and
// test helpers. Keep in sync with schema/clickhouse/migrations/001_create_events_table.sql.
const EventsInsertStmt = `INSERT INTO events (` + EventsInsertColumns + `)`

// PromotedAutoRow holds the typed values for promoted auto-property columns on
// one events row. Zero values match ClickHouse DEFAULTs (empty strings; false
// for Mobile; nil for BotScore and VerifiedBot).
type PromotedAutoRow struct {
	BotScore *uint8
	// VerifiedBot uses *bool because the CF-Verified-Bot header is opt-in:
	// nil distinguishes "header absent" from "Cloudflare said not-verified".
	// Mobile is plain bool because absence and false collapse to the same
	// meaning ("not mobile") by convention.
	VerifiedBot    *bool
	Mobile         bool
	Country        string
	Region         string
	City           string
	Browser        string
	BrowserVersion string
	OS             string
	OSVersion      string
	Device         string
	Platform       string
	URL            string
	UTMSource      string
	UTMMedium      string
	UTMCampaign    string
	Pathname       string
	Hostname       string
	Referrer       string
	ReferrerDomain string
	Channel        string
	Locale         string
	ScreenSize     string
	UTMTerm        string
	UTMContent     string
	PageTitle      string
}

// AppendArgs returns promoted column values in EventsInsertPromotedColumns order.
func (r PromotedAutoRow) AppendArgs() []any {
	return []any{
		r.BotScore,
		r.VerifiedBot,
		r.Mobile,
		r.Country,
		r.Region,
		r.City,
		r.Browser,
		r.BrowserVersion,
		r.OS,
		r.OSVersion,
		r.Device,
		r.Platform,
		r.URL,
		r.UTMSource,
		r.UTMMedium,
		r.UTMCampaign,
		r.Pathname,
		r.Hostname,
		r.Referrer,
		r.ReferrerDomain,
		r.Channel,
		r.Locale,
		r.ScreenSize,
		r.UTMTerm,
		r.UTMContent,
		r.PageTitle,
	}
}

// ScanDest returns scan destinations in EventsInsertPromotedColumns order.
func (r *PromotedAutoRow) ScanDest() []any {
	return []any{
		&r.BotScore,
		&r.VerifiedBot,
		&r.Mobile,
		&r.Country,
		&r.Region,
		&r.City,
		&r.Browser,
		&r.BrowserVersion,
		&r.OS,
		&r.OSVersion,
		&r.Device,
		&r.Platform,
		&r.URL,
		&r.UTMSource,
		&r.UTMMedium,
		&r.UTMCampaign,
		&r.Pathname,
		&r.Hostname,
		&r.Referrer,
		&r.ReferrerDomain,
		&r.Channel,
		&r.Locale,
		&r.ScreenSize,
		&r.UTMTerm,
		&r.UTMContent,
		&r.PageTitle,
	}
}

// MergeIntoAutoProperties overlays promoted column values onto m using the
// canonical auto-property keys. Existing map entries win when non-empty.
// Nil nullable fields (BotScore, VerifiedBot) and zero-valued non-nullable
// bools (Mobile=false) are skipped to match the pre-promotion behavior where
// absent map keys did not appear.
func (r PromotedAutoRow) MergeIntoAutoProperties(m map[string]any) map[string]any {
	if m == nil {
		m = make(map[string]any, len(promotedAutoColumns))
	}
	setString := func(key, val string) {
		if val == "" {
			return
		}
		if existing, ok := m[key].(string); ok && existing != "" {
			return
		}
		m[key] = val
	}
	setString(autoprop.PropBotScore, botScoreString(r.BotScore))
	if r.VerifiedBot != nil {
		setString(autoprop.PropVerifiedBot, boolString(*r.VerifiedBot))
	}
	if r.Mobile {
		setString(autoprop.PropMobile, "true")
	}
	for _, col := range promotedAutoColumns {
		if col.Str != nil {
			setString(col.Property, *col.Str(&r))
		}
	}
	return m
}

func boolString(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

func botScoreString(v *uint8) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%d", *v)
}

// SplitPromotedAutoProperties extracts promoted keys from a proto auto-property
// map into PromotedAutoRow and returns the remainder for auto_properties storage.
func SplitPromotedAutoProperties(src map[string]*commonv1.PropertyValue) (PromotedAutoRow, map[string]*commonv1.PropertyValue) {
	if len(src) == 0 {
		return PromotedAutoRow{}, nil
	}
	var row PromotedAutoRow
	rest := make(map[string]*commonv1.PropertyValue, len(src))
	for k, v := range src {
		col, promoted := promotedAutoByProperty[k]
		if !promoted {
			rest[k] = v
			continue
		}
		if !applyPromotedPropertyValue(&row, col, v) {
			rest[k] = v
		}
	}
	if len(rest) == 0 {
		rest = nil
	}
	return row, rest
}

func applyPromotedPropertyValue(row *PromotedAutoRow, col PromotedAutoColumn, pv *commonv1.PropertyValue) bool {
	if pv == nil {
		return false
	}
	switch col.Kind {
	case PromotedString:
		s, ok := autoprop.String(pv)
		if !ok {
			return false
		}
		setPromotedString(row, col, s)
	case PromotedBool:
		b, ok := boolFromPropertyValue(pv)
		if !ok {
			return false
		}
		setPromotedBool(row, col.Property, b)
	case PromotedNullableBool:
		b, ok := boolFromPropertyValue(pv)
		if !ok {
			return false
		}
		setPromotedNullableBool(row, col.Property, b)
	case PromotedNullableUInt8:
		n, ok := intFromPropertyValue(pv)
		if !ok || n < 0 || n > 255 {
			return false
		}
		v := uint8(n)
		row.BotScore = &v
	}
	return true
}

// setPromotedString writes a value into the row field col addresses. A
// PromotedString column with no Str accessor is unrepresentable in practice —
// TestPromotedStringColumnsHaveAccessors rejects the table entry rather than
// letting the write silently vanish here, which is what the hand-written
// switch this replaced did on a forgotten case.
func setPromotedString(row *PromotedAutoRow, col PromotedAutoColumn, value string) {
	if col.Str != nil {
		*col.Str(row) = value
	}
}

func setPromotedBool(row *PromotedAutoRow, property string, value bool) {
	if property == autoprop.PropMobile {
		row.Mobile = value
	}
}

func setPromotedNullableBool(row *PromotedAutoRow, property string, value bool) {
	if property == autoprop.PropVerifiedBot {
		row.VerifiedBot = &value
	}
}

// SplitPromotedAutoAnyProperties extracts promoted keys from a seed/test
// map[string]any auto-property map.
func SplitPromotedAutoAnyProperties(src map[string]any) (PromotedAutoRow, map[string]any) {
	if len(src) == 0 {
		return PromotedAutoRow{}, nil
	}
	var row PromotedAutoRow
	rest := make(map[string]any, len(src))
	for k, v := range src {
		col, promoted := promotedAutoByProperty[k]
		if !promoted {
			rest[k] = v
			continue
		}
		if !applyPromotedAnyValue(&row, col, v) {
			rest[k] = v
		}
	}
	if len(rest) == 0 {
		rest = nil
	}
	return row, rest
}

func applyPromotedAnyValue(row *PromotedAutoRow, col PromotedAutoColumn, v any) bool {
	switch col.Kind {
	case PromotedString:
		s, ok := anyToString(v)
		if !ok || s == "" {
			return ok && s == ""
		}
		setPromotedString(row, col, s)
	case PromotedBool:
		b, ok := anyToBool(v)
		if !ok {
			return false
		}
		setPromotedBool(row, col.Property, b)
	case PromotedNullableBool:
		b, ok := anyToBool(v)
		if !ok {
			return false
		}
		setPromotedNullableBool(row, col.Property, b)
	case PromotedNullableUInt8:
		n, ok := anyToInt64(v)
		if !ok || n < 0 || n > 255 {
			return false
		}
		u := uint8(n)
		row.BotScore = &u
	}
	return true
}

// SplitPromotedAutoVariantMap extracts promoted keys from a Variant auto-property
// map used in tests and returns the remainder for auto_properties storage.
func SplitPromotedAutoVariantMap(src map[string]chcol.Variant) (PromotedAutoRow, map[string]chcol.Variant) {
	if len(src) == 0 {
		return PromotedAutoRow{}, nil
	}
	var row PromotedAutoRow
	rest := make(map[string]chcol.Variant, len(src))
	for k, v := range src {
		col, promoted := promotedAutoByProperty[k]
		if !promoted {
			rest[k] = v
			continue
		}
		if !applyPromotedVariantValue(&row, col, v) {
			rest[k] = v
		}
	}
	if len(rest) == 0 {
		rest = nil
	}
	return row, rest
}

func applyPromotedVariantValue(row *PromotedAutoRow, col PromotedAutoColumn, v chcol.Variant) bool {
	switch col.Kind {
	case PromotedString:
		s, ok := v.Any().(string)
		if !ok {
			return false
		}
		setPromotedString(row, col, s)
	case PromotedBool:
		b, ok := v.Any().(bool)
		if !ok {
			return false
		}
		setPromotedBool(row, col.Property, b)
	case PromotedNullableBool:
		b, ok := v.Any().(bool)
		if !ok {
			return false
		}
		setPromotedNullableBool(row, col.Property, b)
	case PromotedNullableUInt8:
		n, ok := v.Any().(int64)
		if !ok || n < 0 || n > 255 {
			return false
		}
		u := uint8(n)
		row.BotScore = &u
	}
	return true
}

func boolFromPropertyValue(pv *commonv1.PropertyValue) (bool, bool) {
	switch v := pv.GetValue().(type) {
	case *commonv1.PropertyValue_BoolValue:
		return v.BoolValue, true
	case *commonv1.PropertyValue_StringValue:
		b, err := parseBoolString(v.StringValue)
		return b, err == nil
	default:
		return false, false
	}
}

func intFromPropertyValue(pv *commonv1.PropertyValue) (int64, bool) {
	switch v := pv.GetValue().(type) {
	case *commonv1.PropertyValue_IntValue:
		return v.IntValue, true
	case *commonv1.PropertyValue_StringValue:
		var n int64
		_, err := fmt.Sscan(v.StringValue, &n)
		return n, err == nil
	default:
		return 0, false
	}
}

func anyToString(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case fmt.Stringer:
		return x.String(), true
	default:
		return "", false
	}
}

func anyToBool(v any) (bool, bool) {
	switch x := v.(type) {
	case bool:
		return x, true
	case string:
		b, err := parseBoolString(x)
		return b, err == nil
	default:
		return false, false
	}
}

func anyToInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case int:
		return int64(x), true
	case int64:
		return x, true
	case float64:
		return int64(x), true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	default:
		return 0, false
	}
}

func parseBoolString(s string) (bool, error) {
	switch s {
	case "true", "1":
		return true, nil
	case "false", "0", "":
		return false, nil
	default:
		return false, fmt.Errorf("invalid bool string %q", s)
	}
}

// PrepareEventInsertArgs builds positional INSERT args for the events table in
// EventsInsertColumns order. Promoted auto-property keys are stripped from
// the auto_properties map and written to dedicated columns.
func PrepareEventInsertArgs(
	eventID, projectID, distinctID, kind string,
	auto, custom map[string]chcol.Variant,
	occurTime any,
	sessionID string,
) []any {
	promoted, restAuto := SplitPromotedAutoVariantMap(auto)
	args := []any{eventID, projectID, distinctID, kind, restAuto, custom}
	args = append(args, promoted.AppendArgs()...)
	args = append(args, occurTime, sessionID)
	return args
}
