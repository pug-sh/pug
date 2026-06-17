package insights

import (
	"context"
	"errors"
	"testing"
	"time"

	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
	"google.golang.org/protobuf/proto"
)

// A valid-charset but unloadable zone passes the proto charset pattern yet must be
// rejected up front by ExecuteQuery's tzx.Validate as a client InvalidQueryError —
// before any builder dispatch or executor access (hence the empty Executor is safe).
func TestExecuteQuery_RejectsUnloadableTimezone(t *testing.T) {
	req := &insightsv1.QueryRequest{
		Spec:     &insightsv1.InsightQuerySpec{InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum()},
		Timezone: proto.String("Not/A/Zone"),
	}
	_, err := ExecuteQuery(context.Background(), &Executor{}, "proj_123", req, time.Now())
	var invalid *InvalidQueryError
	if !errors.As(err, &invalid) {
		t.Fatalf("err = %v (%T), want *InvalidQueryError", err, err)
	}
}
