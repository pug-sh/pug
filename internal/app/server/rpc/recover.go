package rpc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"

	"connectrpc.com/connect"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/slogx"
)

// RecoverHandlerPanic is the connect.WithRecover hook for every served handler.
// It turns a panic escaping an RPC handler into a CodeInternal error, logging the
// value plus stack and recording it on the span. The client-facing message is
// deliberately generic — a panic value or stack trace must never reach an API
// consumer.
//
// connect's own docs call this option unnecessary for crash prevention, because
// net/http recovers handler panics per connection. That assumption does not hold
// on the /mcp loopback, where the handler is invoked in-process on a go-sdk
// jsonrpc2 goroutine that net/http's recover never reaches — there, an escaping
// panic terminates the process. This is the first line of defence and the only
// one that yields a proper error code and telemetry; mcp.loopbackClient.Do keeps a
// second recover for panics outside the Connect chain (connect deliberately
// re-panics on http.ErrAbortHandler, and the authn middleware and mux routing sit
// outside this hook entirely).
func RecoverHandlerPanic(ctx context.Context, spec connect.Spec, _ http.Header, p any) error {
	err := fmt.Errorf("panic in RPC handler: %v", p)
	slog.ErrorContext(ctx, "recovered panic in RPC handler",
		slogx.Error(err),
		slog.String("procedure", spec.Procedure),
		slog.String("stack", string(debug.Stack())))
	telemetry.RecordError(ctx, err)

	return connect.NewError(connect.CodeInternal, errors.New("internal error"))
}
