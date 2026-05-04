package insights

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	protovalidatemw "connectrpc.com/validate"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
	"github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1/insightsv1connect"
)

// TestValidateInterceptor_RejectsInvalidQueryRequest is a seam test: it verifies that when a
// Connect handler is wired with `validate.NewInterceptor()`, a malformed request is rejected
// at the interceptor layer before ever reaching the handler business logic. All other
// `*_validate_test.go` files call `protovalidate.Validate` directly, which proves the CEL
// annotations work but not that the interceptor is actually wired — that is the contract this
// test pins.
func TestValidateInterceptor_RejectsInvalidQueryRequest(t *testing.T) {
	interceptor := protovalidatemw.NewInterceptor()

	handlerCalled := false
	impl := &interceptorTestServer{onQuery: func() { handlerCalled = true }}
	_, handler := insightsv1connect.NewInsightsServiceHandler(impl, connect.WithInterceptors(interceptor))

	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := insightsv1connect.NewInsightsServiceClient(http.DefaultClient, ts.URL)

	// A QueryRequest with insight_type=UNSPECIFIED violates enum.not_in:[0].
	req := &insightsv1.QueryRequest{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_UNSPECIFIED.Enum(),
		Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
		TimeRange: &commonv1.TimeRange{
			From: timestamppb.New(time.Now().Add(-time.Hour)),
			To:   timestamppb.Now(),
		},
		Events: []*insightsv1.EventQuery{{Event: &commonv1.EventFilter{Kind: proto.String("signup")}}},
	}
	_, callErr := client.Query(context.Background(), connect.NewRequest(req))

	if callErr == nil {
		t.Fatal("expected error, got nil")
	}
	if got := connect.CodeOf(callErr); got != connect.CodeInvalidArgument {
		t.Errorf("got code %v, want CodeInvalidArgument", got)
	}
	if handlerCalled {
		t.Error("handler was called — interceptor did not short-circuit")
	}

	// Sanity-check: the rejection is from the CEL validation rule, not some other arbitrary
	// InvalidArgument. Across the wire the ValidationError type is flattened into the
	// connect.Error message, so we match on the serialized text.
	msg := callErr.Error()
	if !strings.Contains(msg, "insight_type") || !strings.Contains(msg, "not be in list") {
		t.Errorf("expected insight_type/not_in rejection message, got: %v", callErr)
	}
}

// interceptorTestServer is a minimal InsightsServiceHandler stub used only by the interceptor
// seam test — it records whether the Query handler was invoked. Other methods are inherited
// from UnimplementedInsightsServiceHandler, which returns unimplemented.
type interceptorTestServer struct {
	onQuery func()
	insightsv1connect.UnimplementedInsightsServiceHandler
}

func (s *interceptorTestServer) Query(
	ctx context.Context,
	req *connect.Request[insightsv1.QueryRequest],
) (*connect.Response[insightsv1.QueryResponse], error) {
	s.onQuery()
	return connect.NewResponse(&insightsv1.QueryResponse{}), nil
}
