// Package autoprop provides server-controlled typed enrichment for
// auto-property values. Maps each known auto-property key to its expected
// typed PropertyValue / Variant slot — bot scores → Int64, lat/long →
// Float64, verified-bot/mobile → Bool — so downstream Variant columns get
// correctly-typed slots and the dashboard's filter UI surfaces the right
// type per property.
//
// When a known typed key receives a value that fails the typed parse,
// PropertyValue and Variant fall back to the String slot AND emit
// events.property_dropped_total{stage="enrichment", reason="parse_failed"}
// against the supplied ctx so the rate is observable. The OTel default
// meter is a no-op when no exporter is wired, so non-production callers
// (CSV/seed playback, tests) can pass context.Background().
package autoprop

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/ClickHouse/clickhouse-go/v2/lib/chcol"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/pug-sh/pug/internal/deps/telemetry"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	"github.com/pug-sh/pug/internal/geo"
	"github.com/pug-sh/pug/internal/slogx"
)

const (
	PropBotScore     = "$bot_score"
	PropVerifiedBot  = "$verified_bot"
	PropScreenWidth  = "$screenWidth"
	PropScreenHeight = "$screenHeight"
	PropMobile       = "$mobile"
)

// Lat/long key strings are owned by the geo package. Re-exporting them here
// (instead of redefining) keeps a single source of truth so a rename in geo
// can't silently fall through to autoprop's String default.
const (
	PropLatitude  = geo.PropLatitude
	PropLongitude = geo.PropLongitude
)

var propertyDroppedCounter metric.Int64Counter

func init() {
	meter := otel.Meter("github.com/pug-sh/pug/internal/autoprop")
	propertyDroppedCounter, _ = meter.Int64Counter(
		"events.property_dropped_total",
		metric.WithDescription("Per-property enrichment failures. stage=enrichment, reason=parse_failed when a known typed key's value couldn't be parsed and was coerced to a String slot."),
	)
}

// PropertyValue returns the typed PropertyValue for a known auto-property
// key, or a StringValue fallback. When a known typed key's value fails the
// expected parse, records events.property_dropped_total{reason="parse_failed"}
// against the supplied ctx.
func PropertyValue(ctx context.Context, projectID, key, value string) *commonv1.PropertyValue {
	switch key {
	case PropVerifiedBot, PropMobile:
		if b, err := strconv.ParseBool(value); err == nil {
			return &commonv1.PropertyValue{Value: &commonv1.PropertyValue_BoolValue{BoolValue: b}}
		}
		recordParseFailed(ctx, projectID, key, value)
	case PropBotScore, PropScreenWidth, PropScreenHeight:
		if n, err := strconv.ParseInt(value, 10, 64); err == nil {
			return &commonv1.PropertyValue{Value: &commonv1.PropertyValue_IntValue{IntValue: n}}
		}
		recordParseFailed(ctx, projectID, key, value)
	case PropLatitude, PropLongitude:
		if f, err := strconv.ParseFloat(value, 64); err == nil {
			return &commonv1.PropertyValue{Value: &commonv1.PropertyValue_DoubleValue{DoubleValue: f}}
		}
		recordParseFailed(ctx, projectID, key, value)
	}

	return &commonv1.PropertyValue{Value: &commonv1.PropertyValue_StringValue{StringValue: value}}
}

// String renders a PropertyValue as the string the promotion layer stores
// (clickhouse.SplitPromotedAutoProperties) and the ingest enrichers derive
// from (attribution.Derive). Reports false when the value is absent or holds
// no slot. It is the inverse direction of PropertyValue and lives beside it
// for the same reason: two copies would drift silently, and these two callers
// must not disagree — the SDK handler derives $channel/$pathname from this
// string while the storage layer files that same value into a promoted
// column, so a divergence would mean a row whose derived columns describe a
// value the row does not contain.
func String(pv *commonv1.PropertyValue) (string, bool) {
	switch v := pv.GetValue().(type) {
	case *commonv1.PropertyValue_StringValue:
		return v.StringValue, true
	case *commonv1.PropertyValue_IntValue:
		return strconv.FormatInt(v.IntValue, 10), true
	case *commonv1.PropertyValue_DoubleValue:
		return strconv.FormatFloat(v.DoubleValue, 'g', -1, 64), true
	case *commonv1.PropertyValue_BoolValue:
		return strconv.FormatBool(v.BoolValue), true
	default:
		return "", false
	}
}

// Variant returns the chcol.Variant slot for a known auto-property key.
// Delegates to PropertyValue so the key→type mapping has a single source of
// truth, then maps the resulting oneof case to a Variant slot.
func Variant(ctx context.Context, projectID, key, value string) chcol.Variant {
	pv := PropertyValue(ctx, projectID, key, value)
	switch v := pv.GetValue().(type) {
	case *commonv1.PropertyValue_IntValue:
		return chcol.NewVariantWithType(v.IntValue, "Int64")
	case *commonv1.PropertyValue_DoubleValue:
		return chcol.NewVariantWithType(v.DoubleValue, "Float64")
	case *commonv1.PropertyValue_BoolValue:
		return chcol.NewVariantWithType(v.BoolValue, "Bool")
	case *commonv1.PropertyValue_StringValue:
		return chcol.NewVariantWithType(v.StringValue, "String")
	}
	return chcol.NewVariantWithType(value, "String")
}

func recordParseFailed(ctx context.Context, projectID, key, value string) {
	err := fmt.Errorf("autoprop: parse failed for typed key %q (value=%q), falling back to String slot", key, value)
	slog.WarnContext(ctx, "autoprop: typed key parse failed, coercing to String",
		slogx.Error(err),
		slog.String("project_id", projectID),
		slog.String("property_key", key))
	telemetry.RecordError(ctx, err)
	propertyDroppedCounter.Add(ctx, 1, metric.WithAttributes(
		attribute.String("project_id", projectID),
		attribute.String("stage", "enrichment"),
		attribute.String("reason", "parse_failed"),
	))
}
