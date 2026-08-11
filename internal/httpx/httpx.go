package httpx

import (
	"net/http"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

const defaultTimeout = 30 * time.Second

// NewClient returns a client for calling a third party. Its requests carry a
// span, without which a slow dependency shows up only as an unexplained gap in
// the calling handler's trace.
func NewClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: otelhttp.NewTransport(http.DefaultTransport),
	}
}
