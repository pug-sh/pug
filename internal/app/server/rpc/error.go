package rpc

import (
	"context"
	"errors"
	"log/slog"
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

	// Handler-tagged error: build the connect error and attach its stable reason.
	if appErr, ok := errors.AsType[*apperr.Error](err); ok {
		cerr := connect.NewError(appErr.Code, errors.New(appErr.Message))
		attachDetails(ctx, cerr, appErr.Reason)
		for _, d := range appErr.Details() {
			addDetail(ctx, cerr, d)
		}
		return cerr
	}

	// Already a connect error: attach a generic reason mapped from the code.
	if connectErr, ok := errors.AsType[*connect.Error](err); ok {
		attachDetails(ctx, connectErr, apperr.ReasonForCode(connectErr.Code()))
		if vErr, ok := errors.AsType[*protovalidate.ValidationError](connectErr); ok {
			addDetail(ctx, connectErr, badRequestFromViolations(vErr))
		}
		return connectErr
	}

	// Let Connect RPC map context errors to the correct codes (no detail).
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Non-connect error - log and sanitize.
	slog.ErrorContext(ctx, "unhandled error",
		slog.String("procedure", procedure),
		slogx.Error(err))
	cerr := connect.NewError(connect.CodeInternal, errors.New("internal error"))
	attachDetails(ctx, cerr, apperr.ReasonInternal)
	return cerr
}

// attachDetails attaches the standard google.rpc error details: an ErrorInfo
// (reason + domain, trace id in metadata) and a RequestInfo carrying the
// per-request correlation id. The id/trace/domain are sourced here (one place)
// so the response id always matches the logged id.
func attachDetails(ctx context.Context, cerr *connect.Error, reason string) {
	info := &errdetails.ErrorInfo{Reason: reason, Domain: apperr.Domain}
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

// badRequestFromViolations maps protovalidate violations to a google.rpc.BadRequest.
func badRequestFromViolations(vErr *protovalidate.ValidationError) *errdetails.BadRequest {
	br := &errdetails.BadRequest{}
	for _, v := range vErr.ToProto().GetViolations() {
		br.FieldViolations = append(br.FieldViolations, &errdetails.BadRequest_FieldViolation{
			Field:       fieldPathString(v.GetField()),
			Description: v.GetMessage(),
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
