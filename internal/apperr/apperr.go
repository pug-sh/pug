// Package apperr defines application errors tagged with a stable reason code.
// The correlation id and the google.rpc error details are attached centrally by
// the RPC error interceptor; this package only carries the {code, reason,
// message} triple and owns the reason registry.
package apperr

import (
	"fmt"
	"regexp"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
)

// Domain namespaces all reason codes (google.rpc.ErrorInfo style).
const Domain = "pug.sh"

// Error is an application error tagged with a stable, public reason code.
type Error struct {
	Code    connect.Code
	Reason  string
	Message string
	details []proto.Message
}

func (e *Error) Error() string { return e.Message }

// Details returns the google.rpc errdetails payloads to attach to the response.
func (e *Error) Details() []proto.Message { return e.details }

// Option augments an Error with typed google.rpc error details.
type Option func(*Error)

// Err builds a tagged application error. Pass a registered Reason* value.
func Err(code connect.Code, reason, message string, opts ...Option) error {
	e := &Error{Code: code, Reason: reason, Message: message}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// reasonFormat is canonical UPPER_SNAKE_CASE: a leading letter, then
// underscore-separated runs of letters/digits. Rejects leading/trailing/double
// underscores so a typo cannot become a permanent public reason code.
var reasonFormat = regexp.MustCompile(`^[A-Z][A-Z0-9]*(_[A-Z0-9]+)*$`)

// reasonRegistry enforces format + uniqueness of reason codes at package init.
type reasonRegistry struct{ seen map[string]struct{} }

func (r *reasonRegistry) add(code string) string {
	if !reasonFormat.MatchString(code) {
		panic(fmt.Sprintf("apperr: invalid reason code %q (want UPPER_SNAKE_CASE)", code))
	}
	if _, dup := r.seen[code]; dup {
		panic(fmt.Sprintf("apperr: duplicate reason code %q", code))
	}
	r.seen[code] = struct{}{}
	return code
}

var codes = &reasonRegistry{seen: map[string]struct{}{}}

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

// Domain reasons — specific, client-facing meanings. Immutable once shipped:
// never rename, only add. Add new ones here as handlers adopt them.
var (
	ReasonProfileNotFound = codes.add("PROFILE_NOT_FOUND")
)

// ReasonForCode returns the generic reason for a Connect code (fallback for
// errors that were not tagged with a specific reason).
func ReasonForCode(c connect.Code) string {
	switch c {
	case connect.CodeUnknown:
		return ReasonUnknown
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
