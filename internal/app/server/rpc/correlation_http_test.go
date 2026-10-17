package rpc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/authn"
	"connectrpc.com/connect"
	"github.com/pug-sh/pug/internal/apperr"
	"github.com/pug-sh/pug/internal/correlation"
	"google.golang.org/protobuf/types/known/emptypb"
)

// An auth rejection is written OUTSIDE the Connect interceptor chain. With
// WithCorrelationID mounted outside the authn middleware and authn errors built
// via unauthenticated() or unavailable(), the client still receives ErrorInfo
// (reason) and RequestInfo (error_id), the same envelope as handler errors.
func TestAuthRejectionCarriesErrorDetails(t *testing.T) {
	tests := map[string]struct {
		reject func(context.Context, string) error
		code   connect.Code
		reason apperr.Reason
	}{
		"unauthenticated": {reject: unauthenticated, code: connect.CodeUnauthenticated, reason: apperr.ReasonUnauthenticated},
		"unavailable":     {reject: unavailable, code: connect.CodeUnavailable, reason: apperr.ReasonUnavailable},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			authMW := authn.NewMiddleware(func(ctx context.Context, _ *http.Request) (any, error) {
				return nil, tc.reject(ctx, "no token")
			})

			const path = "/test.v1.Svc/Method"
			connectHandler := connect.NewUnaryHandler(
				path,
				func(_ context.Context, _ *connect.Request[emptypb.Empty]) (*connect.Response[emptypb.Empty], error) {
					return connect.NewResponse(&emptypb.Empty{}), nil // never reached: auth fails first
				},
			)

			mux := http.NewServeMux()
			mux.Handle(path, WithCorrelationID(authMW.Wrap(connectHandler)))
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)

			client := connect.NewClient[emptypb.Empty, emptypb.Empty](srv.Client(), srv.URL+path)
			_, err := client.CallUnary(context.Background(), connect.NewRequest(&emptypb.Empty{}))
			if got := connect.CodeOf(err); got != tc.code {
				t.Fatalf("code = %v (err %v), want %v", got, err, tc.code)
			}
			info := errorInfoFrom(t, err)
			if info == nil || info.GetReason() != string(tc.reason) {
				t.Errorf("ErrorInfo = %+v, want reason %s", info, tc.reason)
			}
			if ri := requestInfoFrom(t, err); ri == nil || ri.GetRequestId() == "" {
				t.Errorf("RequestInfo = %+v, want non-empty error_id", ri)
			}
		})
	}
}

// WithCorrelationID mints an id when none is present, and preserves an existing one.
func TestWithCorrelationID(t *testing.T) {
	t.Run("mints when absent", func(t *testing.T) {
		var seen string
		h := WithCorrelationID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			seen = correlation.IDFromContext(r.Context())
		}))
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil))
		if seen == "" {
			t.Error("expected a correlation id to be minted")
		}
	})

	t.Run("preserves existing", func(t *testing.T) {
		var seen string
		h := WithCorrelationID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			seen = correlation.IDFromContext(r.Context())
		}))
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req = req.WithContext(correlation.WithID(req.Context(), "preset"))
		h.ServeHTTP(httptest.NewRecorder(), req)
		if seen != "preset" {
			t.Errorf("id = %q, want preset", seen)
		}
	})
}
