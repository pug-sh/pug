// Package apperr defines application errors tagged with a stable reason code.
// The correlation id is attached centrally by the RPC error interceptor; this
// package carries the {code, reason, message} triple plus optional typed
// google.rpc detail payloads, and owns the reason registry.
package apperr

import (
	"fmt"
	"regexp"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
)

// Domain namespaces all reason codes (google.rpc.ErrorInfo style).
const Domain = "pug.sh"

// Reason is a stable, public error reason code. The only valid values are those
// minted by codes.add in a reasons_*.go file; Err panics on any other value, so
// a Reason that reaches an Error is always registered, format-valid, and unique.
type Reason string

// Error is an application error tagged with a stable, public reason code. It is
// immutable once built by Err: the fields are unexported and read through
// accessors, so neither a consumer holding the recovered *Error nor an Option
// can rewrite the code/reason/message after construction.
type Error struct {
	code    connect.Code
	reason  Reason
	message string
	details []proto.Message
}

func (e *Error) Error() string { return e.message }

// Code returns the Connect code the error maps to.
func (e *Error) Code() connect.Code { return e.code }

// Reason returns the stable, registered reason code.
func (e *Error) Reason() Reason { return e.reason }

// Message returns the client-facing message.
func (e *Error) Message() string { return e.message }

// Details returns the google.rpc errdetails payloads to attach to the response.
func (e *Error) Details() []proto.Message { return e.details }

// detailSink is the only mutation surface exposed to Options. It can accumulate
// google.rpc detail payloads but cannot touch the Error's code/reason/message,
// which keeps those invariant-bearing fields construct-only.
type detailSink struct{ details []proto.Message }

// Option augments an Error with typed google.rpc error details.
type Option func(*detailSink)

// Err builds a tagged application error. Pass a registered Reason* value; an
// unregistered reason panics (see the construction-time guard below).
func Err(code connect.Code, reason Reason, message string, opts ...Option) error {
	if !codes.registered(reason) {
		panic(fmt.Sprintf("apperr: unregistered reason %q (declare it via codes.add in a reasons_*.go file)", reason))
	}
	var sink detailSink
	for _, opt := range opts {
		opt(&sink)
	}
	return &Error{code: code, reason: reason, message: message, details: sink.details}
}

// reasonFormat is canonical UPPER_SNAKE_CASE: a leading letter, then
// underscore-separated runs of letters/digits. Rejects leading/trailing/double
// underscores so a typo cannot become a permanent public reason code.
var reasonFormat = regexp.MustCompile(`^[A-Z][A-Z0-9]*(_[A-Z0-9]+)*$`)

// reasonRegistry enforces format + uniqueness of reason codes at package init.
type reasonRegistry struct{ seen map[Reason]struct{} }

func (r *reasonRegistry) add(code string) Reason {
	if !reasonFormat.MatchString(code) {
		panic(fmt.Sprintf("apperr: invalid reason code %q (want UPPER_SNAKE_CASE)", code))
	}
	reason := Reason(code)
	if _, dup := r.seen[reason]; dup {
		panic(fmt.Sprintf("apperr: duplicate reason code %q", code))
	}
	r.seen[reason] = struct{}{}
	return reason
}

// registered reports whether reason was minted via add (i.e. is a valid public
// reason). Err uses it to reject unregistered reasons at construction time.
func (r *reasonRegistry) registered(reason Reason) bool {
	_, ok := r.seen[reason]
	return ok
}

var codes = &reasonRegistry{seen: map[Reason]struct{}{}}

// Generic reasons — mirror Connect codes, used as fallbacks for untagged errors.
var (
	ReasonUnknown            = codes.add("UNKNOWN")
	ReasonCanceled           = codes.add("CANCELED")
	ReasonInvalidArgument    = codes.add("INVALID_ARGUMENT")
	ReasonDeadlineExceeded   = codes.add("DEADLINE_EXCEEDED")
	ReasonNotFound           = codes.add("NOT_FOUND")
	ReasonAlreadyExists      = codes.add("ALREADY_EXISTS")
	ReasonPermissionDenied   = codes.add("PERMISSION_DENIED")
	ReasonResourceExhausted  = codes.add("RESOURCE_EXHAUSTED")
	ReasonFailedPrecondition = codes.add("FAILED_PRECONDITION")
	ReasonAborted            = codes.add("ABORTED")
	ReasonOutOfRange         = codes.add("OUT_OF_RANGE")
	ReasonUnimplemented      = codes.add("UNIMPLEMENTED")
	ReasonInternal           = codes.add("INTERNAL")
	ReasonUnavailable        = codes.add("UNAVAILABLE")
	ReasonDataLoss           = codes.add("DATA_LOSS")
	ReasonUnauthenticated    = codes.add("UNAUTHENTICATED")
)

// ReasonForCode returns the generic reason for a Connect code (fallback for
// errors that were not tagged with a specific reason).
func ReasonForCode(c connect.Code) Reason {
	switch c {
	case connect.CodeCanceled:
		return ReasonCanceled
	case connect.CodeInvalidArgument:
		return ReasonInvalidArgument
	case connect.CodeDeadlineExceeded:
		return ReasonDeadlineExceeded
	case connect.CodeNotFound:
		return ReasonNotFound
	case connect.CodeAlreadyExists:
		return ReasonAlreadyExists
	case connect.CodePermissionDenied:
		return ReasonPermissionDenied
	case connect.CodeResourceExhausted:
		return ReasonResourceExhausted
	case connect.CodeFailedPrecondition:
		return ReasonFailedPrecondition
	case connect.CodeAborted:
		return ReasonAborted
	case connect.CodeOutOfRange:
		return ReasonOutOfRange
	case connect.CodeUnimplemented:
		return ReasonUnimplemented
	case connect.CodeInternal:
		return ReasonInternal
	case connect.CodeUnavailable:
		return ReasonUnavailable
	case connect.CodeDataLoss:
		return ReasonDataLoss
	case connect.CodeUnauthenticated:
		return ReasonUnauthenticated
	default:
		return ReasonUnknown
	}
}
