package httpx

import (
	"net"
	"net/http"
	"time"

	"code.dny.dev/ssrf"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

const defaultTimeout = 30 * time.Second

const (
	// Callers read bodies whole, from a server someone else runs.
	maxGuardedBodyBytes = 1 << 20
	// net/http's default allows 10 MiB of headers.
	maxGuardedHeaderBytes = 64 << 10
)

// Company servers often listen on 8443.
var guard = ssrf.New(ssrf.WithAnyPort())

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

// NewGuardedClient returns a client for a URL someone outside pug chose. Unless
// allowPrivate, it refuses private, loopback and metadata addresses at dial time and
// ignores proxies. Every response body is capped at 1 MiB, and its headers at 64 KiB.
func NewGuardedClient(timeout time.Duration, allowPrivate bool) *http.Client {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: otelhttp.NewTransport(cappedTransport{base: guardedTransport(timeout, allowPrivate)}),
	}
}

func guardedTransport(timeout time.Duration, allowPrivate bool) *http.Transport {
	dialer := &net.Dialer{Timeout: timeout}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxResponseHeaderBytes = maxGuardedHeaderBytes
	if !allowPrivate {
		dialer.Control = guard.Safe
		// Behind a proxy the guard would check the proxy's address.
		transport.Proxy = nil
	}
	transport.DialContext = dialer.DialContext
	return transport
}

type cappedTransport struct {
	base http.RoundTripper
}

func (t cappedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	resp.Body = http.MaxBytesReader(nil, resp.Body, maxGuardedBodyBytes)
	return resp, nil
}
