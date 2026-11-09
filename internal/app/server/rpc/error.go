package rpc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"

	validatepb "buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go/buf/validate"
	protovalidate "buf.build/go/protovalidate"
	"connectrpc.com/connect"
	"github.com/pug-sh/pug/internal/apperr"
	"github.com/pug-sh/pug/internal/correlation"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/slogx"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/protobuf/proto"
)

func ErrorInterceptor() connect.Interceptor {
	return &errorInterceptor{}
}

type errorInterceptor struct{}

func (i *errorInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		resp, err := next(ctx, req)
		return resp, sanitizeError(ctx, req.Spec().Procedure, err)
	}
}

func (i *errorInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *errorInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		err := next(ctx, conn)
		return sanitizeError(ctx, conn.Spec().Procedure, err)
	}
}

func sanitizeError(ctx context.Context, procedure string, err error) error {
	if err == nil {
		return nil
	}

	// A done request answers with its context's code, since the cancel or deadline
	// most likely caused the fault. A code the handler chose for the client stands.
	if ctx.Err() != nil && isServerFault(err) {
		err = ConnectCtxErr(ctx.Err())
	}

	// Handler-tagged error: build the connect error and attach its stable reason.
	if appErr, ok := errors.AsType[*apperr.Error](err); ok {
		cerr := connect.NewError(appErr.Code(), errors.New(appErr.Message()))
		attachDetails(ctx, cerr, appErr.Reason())
		for _, d := range appErr.Details() {
			addDetail(ctx, cerr, d)
		}
		return cerr
	}

	// Already a connect error: attach a generic reason mapped from the code.
	if connectErr, ok := errors.AsType[*connect.Error](err); ok {
		vErr, invalid := errors.AsType[*protovalidate.ValidationError](connectErr)
		if invalid {
			connectErr = capViolations(connectErr, vErr)
		}
		attachDetails(ctx, connectErr, apperr.ReasonForCode(connectErr.Code()))
		if invalid {
			addDetail(ctx, connectErr, badRequestFromViolations(vErr))
		}
		return connectErr
	}

	// Non-connect error - log and sanitize.
	slog.ErrorContext(ctx, "unhandled error",
		slog.String("procedure", procedure),
		slogx.Error(err))
	// Last resort for an error no layer classified: otelconnect sets span status
	// but never an exception event, so without this the span carries no cause.
	telemetry.RecordError(ctx, err)
	cerr := connect.NewError(connect.CodeInternal, errors.New("internal error"))
	attachDetails(ctx, cerr, apperr.ReasonInternal)
	return cerr
}

// isServerFault reports a code the server answers for its own failure. An
// *apperr.Error carries its code in a field, which connect.CodeOf does not read,
// and a raw error is Unknown.
func isServerFault(err error) bool {
	code := connect.CodeOf(err)
	if ae, ok := errors.AsType[*apperr.Error](err); ok {
		code = ae.Code()
	}
	return code == connect.CodeUnknown || code == connect.CodeInternal || code == connect.CodeUnavailable
}

// attachDetails attaches the standard google.rpc error details: an ErrorInfo
// (reason + domain, plus trace_id in metadata when a span is active) and, when a
// correlation id is present, a RequestInfo carrying it. The id/trace/domain are
// sourced here (one place) so the response id always matches the logged id.
func attachDetails(ctx context.Context, cerr *connect.Error, reason apperr.Reason) {
	info := &errdetails.ErrorInfo{Reason: string(reason), Domain: apperr.Domain}
	if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() {
		info.Metadata = map[string]string{"trace_id": sc.TraceID().String()}
	}
	addDetail(ctx, cerr, info)

	if id := correlation.IDFromContext(ctx); id != "" {
		addDetail(ctx, cerr, &errdetails.RequestInfo{RequestId: id})
	}
}

func addDetail(ctx context.Context, cerr *connect.Error, msg proto.Message) {
	detail, err := connect.NewErrorDetail(msg)
	if err != nil {
		// Near-impossible (marshaling a small well-known message); log at source.
		slog.ErrorContext(ctx, "failed building error detail", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return
	}
	cerr.AddDetail(detail)
}

// capViolations caps the message the validate interceptor renders from every
// violation; the capped ValidationError stays reachable through errors.As.
func capViolations(cerr *connect.Error, vErr *protovalidate.ValidationError) *connect.Error {
	rest := len(vErr.Violations) - maxViolationLines
	if rest <= 0 {
		return cerr
	}
	shown := &protovalidate.ValidationError{Violations: vErr.Violations[:maxViolationLines]}
	capped := connect.NewError(cerr.Code(), fmt.Errorf("%w\n - and %d more", shown, rest))
	maps.Copy(capped.Meta(), cerr.Meta())
	for _, d := range cerr.Details() {
		capped.AddDetail(d)
	}
	return capped
}

// badRequestFromViolations maps protovalidate violations to a google.rpc.BadRequest,
// at most maxViolationLines of them; the error's message counts the rest.
func badRequestFromViolations(vErr *protovalidate.ValidationError) *errdetails.BadRequest {
	br := &errdetails.BadRequest{}
	for _, v := range vErr.Violations[:min(len(vErr.Violations), maxViolationLines)] {
		br.FieldViolations = append(br.FieldViolations, &errdetails.BadRequest_FieldViolation{
			Field:       fieldPathString(v.Proto.GetField()),
			Description: v.Proto.GetMessage(),
		})
	}
	return br
}

func fieldPathString(fp *validatepb.FieldPath) string {
	if fp == nil {
		return ""
	}
	parts := make([]string, 0, len(fp.GetElements()))
	for _, el := range fp.GetElements() {
		parts = append(parts, el.GetFieldName())
	}
	return strings.Join(parts, ".")
}

// maxViolationLines caps what one validation error adds to a response: the caller
// is untrusted, and one event of empty property values breaks a rule per entry.
const maxViolationLines = 10

// Violations renders a validation error one line per violation, up to
// maxViolationLines and then a count of the rest, plus the first rule broken, for
// a batch that skips an invalid item instead of failing whole.
func Violations(err error) (firstRule string, lines []string) {
	if err == nil {
		return "", nil
	}
	vErr, ok := errors.AsType[*protovalidate.ValidationError](err)
	if !ok || len(vErr.Violations) == 0 {
		return "", []string{err.Error()}
	}
	shown := vErr.Violations[:min(len(vErr.Violations), maxViolationLines)]
	for _, v := range shown {
		lines = append(lines, v.String())
	}
	if rest := len(vErr.Violations) - len(shown); rest > 0 {
		lines = append(lines, fmt.Sprintf("and %d more", rest))
	}
	return vErr.Violations[0].Proto.GetRuleId(), lines
}
