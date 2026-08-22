package usage

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// The instruments are created at package init() against the global meter, whose
// instruments delegate to the real provider once one is installed. OTel binds
// that delegation to the first provider set, so installing ours from TestMain
// keeps a test that calls SetupSDK from winning the race. Counters accumulate
// process-wide, so tests compare deltas.
var testMeterReader = sdkmetric.NewManualReader()

func installTestMeterProvider() {
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(testMeterReader)))
}

// The bool distinguishes "present and zero" from "never wired", so an assertion
// that a counter did not move cannot pass vacuously.
func unrefreshedCount(t *testing.T, reason string) (int64, bool) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := testMeterReader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "usage.unrefreshed_total" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("usage.unrefreshed_total has data type %T, want metricdata.Sum[int64]", m.Data)
			}
			for _, dp := range sum.DataPoints {
				if v, _ := dp.Attributes.Value(attribute.Key("reason")); v.AsString() == reason {
					return dp.Value, true
				}
			}
			return 0, true
		}
	}
	return 0, false
}
