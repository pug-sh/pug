package events

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/ClickHouse/clickhouse-go/v2/lib/chcol"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/pug-sh/pug/internal/deps/telemetry"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	"github.com/pug-sh/pug/internal/slogx"
)

var propertyDroppedCounter metric.Int64Counter

func init() {
	meter := otel.Meter("github.com/pug-sh/pug/internal/app/workers/events")
	propertyDroppedCounter, _ = meter.Int64Counter(
		"events.property_dropped_total",
		metric.WithDescription("Per-property translation failures during proto->Variant conversion. Reason labels: nil, unsupported_variant."),
	)
}

// propertyValueToVariant maps a proto PropertyValue oneof case to the
// corresponding chcol.Variant slot for the events table's
// Map(String, Variant(String, Int64, Float64, Bool, DateTime64(3))) columns.
// The slot names MUST match the migration's Variant declaration; drift is
// caught by the cross-file pin tests in this package and in core/insights.
//
// Returns an error for nil or unset values. Proto-validate's oneof.required
// makes those paths unreachable from the validated RPC ingress, so an error
// here represents proto-future drift or worker-internal bugs and should be
// recorded.
func propertyValueToVariant(pv *commonv1.PropertyValue) (chcol.Variant, error) {
	if pv == nil {
		return chcol.Variant{}, fmt.Errorf("propertyValueToVariant: nil PropertyValue")
	}
	switch v := pv.GetValue().(type) {
	case *commonv1.PropertyValue_StringValue:
		return chcol.NewVariantWithType(v.StringValue, "String"), nil
	case *commonv1.PropertyValue_IntValue:
		return chcol.NewVariantWithType(v.IntValue, "Int64"), nil
	case *commonv1.PropertyValue_DoubleValue:
		return chcol.NewVariantWithType(v.DoubleValue, "Float64"), nil
	case *commonv1.PropertyValue_BoolValue:
		return chcol.NewVariantWithType(v.BoolValue, "Bool"), nil
	case *commonv1.PropertyValue_TimestampValue:
		return chcol.NewVariantWithType(v.TimestampValue.AsTime(), "DateTime64(3)"), nil
	default:
		return chcol.Variant{}, fmt.Errorf("propertyValueToVariant: unsupported PropertyValue variant %T", v)
	}
}

// propertyValueMapToVariantMap converts a proto property map into the
// chcol.Variant map shape the clickhouse-go driver expects for
// Map(String, Variant(...)) columns. Per-property translation failures are
// logged + recorded and the offending key is dropped from the output (the row
// itself is still inserted so the rest of the batch survives). Drops are
// counted via events.property_dropped_total{project_id, reason} so the rate
// is observable independent of log volume.
func propertyValueMapToVariantMap(ctx context.Context, projectID string, src map[string]*commonv1.PropertyValue) map[string]chcol.Variant {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]chcol.Variant, len(src))
	for k, v := range src {
		variant, err := propertyValueToVariant(v)
		if err != nil {
			reason := "unsupported_variant"
			if v == nil {
				reason = "nil"
			}
			slog.WarnContext(ctx, "propertyValueMapToVariantMap: dropping unrepresentable property",
				slogx.Error(err),
				slog.String("project_id", projectID),
				slog.String("property_key", k),
				slog.String("reason", reason))
			telemetry.RecordError(ctx, err)
			propertyDroppedCounter.Add(ctx, 1, metric.WithAttributes(
				attribute.String("project_id", projectID),
				attribute.String("reason", reason),
			))
			continue
		}
		out[k] = variant
	}
	return out
}
