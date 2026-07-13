package mcp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/pug-sh/pug/internal/app/server/rpc"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/slogx"
)

// toolCallTimeout bounds a single in-process tool call.
//
// It exists because the go-sdk dispatches tool handlers on a jsonrpc2 context with
// cancellation detached — Values still resolve, but Done and Deadline do not — so
// an agent that hangs up mid-call cancels nothing, and a wide query_insights would
// keep a ClickHouse query running with nobody waiting for the answer. Every insight
// and profile handler threads its context into the query, so this deadline is what
// stops them. Generous enough for a broad analytics scan, finite enough that
// abandoned work drains.
const toolCallTimeout = 2 * time.Minute

// loopbackClient is a connect.HTTPClient that serves each request in-process
// through the server's own mux instead of over TCP. It injects the caller's
// private API key (stashed in the request context by withAPIKeyPassthrough) as an
// x-api-key header so the inner Connect handler chain re-authenticates and
// re-authorizes the tool call exactly like an external private-key API request —
// validation, authz, otel and logging interceptors all run unchanged, and the
// request context carries the same correlation id end to end (it rides context
// values, which survive the go-sdk's jsonrpc2 hop, rather than the middleware,
// which the loopback re-enters beneath).
//
// The client is a singleton shared by every generated tool closure, so the caller's
// key cannot be a field on it — that would be shared mutable state across
// concurrent tool calls from different projects, i.e. a cross-tenant credential
// leak. Per-request state rides the per-request context, as Principal does.
type loopbackClient struct {
	handler http.Handler // the server mux
}

// Do implements connect.HTTPClient.
func (c *loopbackClient) Do(req *http.Request) (_ *http.Response, err error) {
	ctx := req.Context()

	// A panic escaping the in-process handler would be fatal. On the network path
	// net/http recovers handler panics per connection, but a tool call runs the
	// handler on a go-sdk jsonrpc2 goroutine that recover never reaches, so the panic
	// would kill the process — a multi-tenant outage any private-key holder could
	// trigger. rpc.RecoverHandlerPanic (wired via connect.WithRecover in server.start)
	// is the first line of defence and contains panics inside the Connect chain with a
	// proper code and telemetry; this is the backstop for everything outside it — the
	// authn middleware, mux routing, and the http.ErrAbortHandler panics connect
	// deliberately re-panics. Reporting an error surfaces a failed tool to the model
	// rather than a dropped connection.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("mcp loopback: recovered panic serving %s: %v", req.URL.Path, r)
			slog.ErrorContext(ctx, "recovered panic in mcp loopback handler",
				slogx.Error(err),
				slog.String("path", req.URL.Path),
				slog.String("stack", string(debug.Stack())))
			telemetry.RecordError(ctx, err)
		}
	}()

	// /mcp is private-key-only and withAPIKeyPassthrough stashes the key outside the
	// authn boundary, so every request reaching a tool handler provably carries one.
	// An empty key is therefore unreachable in production and means the endpoint was
	// mounted without the passthrough. Say so: degrading into an anonymous inner
	// request yields a 401 raised outside the interceptor chain, which logs nothing and
	// tells an operator who configured a perfectly good API key that it was rejected.
	key := apiKeyFromContext(ctx)
	if key == "" {
		err = errors.New("mcp loopback: no API key in request context; /mcp was mounted without withAPIKeyPassthrough")
		slog.ErrorContext(ctx, "mcp loopback has no api key to inject", slogx.Error(err))
		telemetry.RecordError(ctx, err)

		return nil, err
	}
	req.Header.Set(rpc.HeaderAPIKey, key)

	ctx, cancel := context.WithTimeout(ctx, toolCallTimeout)
	defer cancel()
	req = req.WithContext(ctx)

	rec := newResponseRecorder()
	c.handler.ServeHTTP(rec, req)

	return rec.result(req), nil
}

// responseRecorder is a minimal http.ResponseWriter that buffers an in-process
// response. Unary Connect calls are non-streaming and the handler writes the status
// at most once, so a plain buffer plus a status code is sufficient; we avoid
// net/http/httptest to keep test-only helpers out of the server binary.
//
// code doubles as the commit flag: 0 means the status has not been written yet, so
// the zero value is already valid and only the first WriteHeader (or the implicit
// commit on the first Write) takes effect — both matching net/http.
//
// It deliberately does NOT implement http.Flusher, and that omission is
// load-bearing rather than an oversight. connect requires a Flusher only for
// server-streaming specs, and no streaming RPC can reach here today
// (protoc-gen-go-mcp generates no tool for one, which is why profiles.List is
// absent). Should a streaming RPC ever be routed through the loopback, connect
// rejects it loudly with "does not implement http.Flusher"; adding a no-op Flush
// would instead buffer the whole stream into memory and hand it back as one blob.
type responseRecorder struct {
	header http.Header
	buf    bytes.Buffer
	code   int // 0 until the status commits; result maps 0 -> 200
}

func newResponseRecorder() *responseRecorder {
	return &responseRecorder{header: make(http.Header)}
}

func (r *responseRecorder) Header() http.Header { return r.header }

func (r *responseRecorder) WriteHeader(code int) {
	if r.code == 0 {
		r.code = code
	}
}

func (r *responseRecorder) Write(p []byte) (int, error) {
	if r.code == 0 {
		r.code = http.StatusOK
	}

	return r.buf.Write(p)
}

func (r *responseRecorder) result(req *http.Request) *http.Response {
	code := r.code
	if code == 0 {
		code = http.StatusOK
	}

	return &http.Response{
		StatusCode: code,
		// The numeric code belongs in Status: connect surfaces this string verbatim as
		// the error message whenever the body is not connect-wire JSON (an inner-mux 404,
		// say), and on that path nothing else is logged — "404 Not Found" is far more
		// actionable to an operator, and to the model, than a bare "Not Found".
		Status:        fmt.Sprintf("%d %s", code, http.StatusText(code)),
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        r.header.Clone(), // snapshot at commit, as net/http does
		Body:          io.NopCloser(bytes.NewReader(r.buf.Bytes())),
		ContentLength: int64(r.buf.Len()),
		Request:       req,
	}
}
