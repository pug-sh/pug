package mcp

import (
	"bytes"
	"io"
	"net/http"

	"github.com/pug-sh/pug/internal/app/server/rpc"
)

// loopbackClient is a connect.HTTPClient that serves each request in-process
// through the server's own mux instead of over TCP. It injects the caller's
// private API key (stashed in the request context by WithAPIKeyPassthrough) as
// an x-api-key header so the inner Connect handler chain re-authenticates and
// re-authorizes the tool call exactly like an external private-key API request —
// validation, authz, otel and logging interceptors all run unchanged, and the
// request context carries the same correlation id end to end.
type loopbackClient struct {
	handler http.Handler // the server mux
}

// Do implements connect.HTTPClient.
func (c *loopbackClient) Do(req *http.Request) (*http.Response, error) {
	if key := apiKeyFromContext(req.Context()); key != "" {
		req.Header.Set(rpc.HeaderAPIKey, key)
	}

	rec := newResponseRecorder()
	c.handler.ServeHTTP(rec, req)
	return rec.result(req), nil
}

// responseRecorder is a minimal http.ResponseWriter that buffers an in-process
// response. Unary Connect calls are non-streaming and the handler writes the
// status at most once, so a plain buffer + status code is sufficient; we avoid
// net/http/httptest to keep test-only helpers out of the server binary. The
// status defaults to 200 (set in newResponseRecorder) when WriteHeader is never
// called, and only the first WriteHeader (or the implicit commit on first Write)
// takes effect — both matching net/http.
type responseRecorder struct {
	header http.Header
	buf    bytes.Buffer
	code   int
	wrote  bool
}

func newResponseRecorder() *responseRecorder {
	return &responseRecorder{header: make(http.Header), code: http.StatusOK}
}

func (r *responseRecorder) Header() http.Header { return r.header }

func (r *responseRecorder) WriteHeader(code int) {
	if r.wrote {
		return
	}
	r.code = code
	r.wrote = true
}

func (r *responseRecorder) Write(p []byte) (int, error) {
	r.wrote = true
	return r.buf.Write(p)
}

func (r *responseRecorder) result(req *http.Request) *http.Response {
	return &http.Response{
		StatusCode: r.code,
		Status:     http.StatusText(r.code),
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     r.header,
		Body:       io.NopCloser(bytes.NewReader(r.buf.Bytes())),
		Request:    req,
	}
}
