package clickhouse

import (
	"fmt"

	"github.com/ClickHouse/clickhouse-go/v2/lib/chcol"

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
type PromotedAutoColumn struct {
	Property string
	Column   string
	Kind     PromotedAutoColumnKind
}

// promotedAutoColumns is the authoritative list of auto-properties extracted
// from auto_properties at ingest. Keep in sync with
// schema/clickhouse/migrations/001_create_events_table.sql.
var promotedAutoColumns = []PromotedAutoColumn{
	{Property: autoprop.PropBotScore, Column: "bot_score", Kind: PromotedNullableUInt8},
	{Property: autoprop.PropVerifiedBot, Column: "verified_bot", Kind: PromotedNullableBool},
	{Property: autoprop.PropMobile, Column: "mobile", Kind: PromotedBool},
	{Property: geo.PropCountry, Column: "country", Kind: PromotedString},
	{Property: geo.PropRegion, Column: "region", Kind: PromotedString},
	{Property: geo.PropCity, Column: "city", Kind: PromotedString},
	{Property: useragent.PropBrowser, Column: "browser", Kind: PromotedString},
	{Property: useragent.PropBrowserVersion, Column: "browser_version", Kind: PromotedString},
	{Property: useragent.PropOS, Column: "os", Kind: PromotedString},
	{Property: useragent.PropOSVersion, Column: "os_version", Kind: PromotedString},
	{Property: useragent.PropDevice, Column: "device", Kind: PromotedString},
	{Property: "$platform", Column: "platform", Kind: PromotedString},
	{Property: "$url", Column: "url", Kind: PromotedString},
	{Property: "$utmSource", Column: "utm_source", Kind: PromotedString},
	{Property: "$utmMedium", Column: "utm_medium", Kind: PromotedString},
	{Property: "$utmCampaign", Column: "utm_campaign", Kind: PromotedString},
}

var promotedAutoByProperty map[string]PromotedAutoColumn

func init() {
	promotedAutoByProperty = make(map[string]PromotedAutoColumn, len(promotedAutoColumns))
	for _, col := range promotedAutoColumns {
		promotedAutoByProperty[col.Property] = col
	}
}

// EventsInsertPromotedColumns lists promoted auto-property columns on the events
// table, in PromotedAutoRow.AppendArgs / ScanDest order.
const EventsInsertPromotedColumns = `bot_score, verified_bot, mobile, country, region, city, browser, browser_version, os, os_version, device, platform, url, utm_source, utm_medium, utm_campaign`

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
	setString(geo.PropCountry, r.Country)
	setString(geo.PropRegion, r.Region)
	setString(geo.PropCity, r.City)
	setString(useragent.PropBrowser, r.Browser)
	setString(useragent.PropBrowserVersion, r.BrowserVersion)
	setString(useragent.PropOS, r.OS)
	setString(useragent.PropOSVersion, r.OSVersion)
	setString(useragent.PropDevice, r.Device)
	setString("$platform", r.Platform)
	setString("$url", r.URL)
	setString("$utmSource", r.UTMSource)
	setString("$utmMedium", r.UTMMedium)
	setString("$utmCampaign", r.UTMCampaign)
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
		s, ok := stringFromPropertyValue(pv)
		if !ok {
			return false
		}
		setPromotedString(row, col.Property, s)
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

func setPromotedString(row *PromotedAutoRow, property, value string) {
	switch property {
	case geo.PropCountry:
		row.Country = value
	case geo.PropRegion:
		row.Region = value
	case geo.PropCity:
		row.City = value
	case useragent.PropBrowser:
		row.Browser = value
	case useragent.PropBrowserVersion:
		row.BrowserVersion = value
	case useragent.PropOS:
		row.OS = value
	case useragent.PropOSVersion:
		row.OSVersion = value
	case useragent.PropDevice:
		row.Device = value
	case "$platform":
		row.Platform = value
	case "$url":
		row.URL = value
	case "$utmSource":
		row.UTMSource = value
	case "$utmMedium":
		row.UTMMedium = value
	case "$utmCampaign":
		row.UTMCampaign = value
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
		setPromotedString(row, col.Property, s)
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
		setPromotedString(row, col.Property, s)
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

func stringFromPropertyValue(pv *commonv1.PropertyValue) (string, bool) {
	switch v := pv.GetValue().(type) {
	case *commonv1.PropertyValue_StringValue:
		return v.StringValue, true
	case *commonv1.PropertyValue_IntValue:
		return fmt.Sprintf("%d", v.IntValue), true
	case *commonv1.PropertyValue_DoubleValue:
		return fmt.Sprintf("%g", v.DoubleValue), true
	case *commonv1.PropertyValue_BoolValue:
		return boolString(v.BoolValue), true
	default:
		return "", false
	}
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
