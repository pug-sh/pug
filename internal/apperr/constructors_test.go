package apperr

import (
	"testing"

	"connectrpc.com/connect"
)

func TestConstructors_setCodeAndReason(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		code   connect.Code
		reason Reason
	}{
		{"NotFound", NotFound(ReasonProfileNotFound, "m"), connect.CodeNotFound, ReasonProfileNotFound},
		{"Invalid", Invalid(ReasonProfileNotFound, "m"), connect.CodeInvalidArgument, ReasonProfileNotFound},
		{"AlreadyExists", AlreadyExists(ReasonProfileNotFound, "m"), connect.CodeAlreadyExists, ReasonProfileNotFound},
		{"PermissionDenied", PermissionDenied(ReasonProfileNotFound, "m"), connect.CodePermissionDenied, ReasonProfileNotFound},
		{"FailedPrecondition", FailedPrecondition(ReasonProfileNotFound, "m"), connect.CodeFailedPrecondition, ReasonProfileNotFound},
		{"Unauthenticated", Unauthenticated(ReasonUnauthenticated, "m"), connect.CodeUnauthenticated, ReasonUnauthenticated},
	}
	for _, c := range cases {
		ae := c.err.(*Error)
		if ae.Code() != c.code {
			t.Errorf("%s: code = %v, want %v", c.name, ae.Code(), c.code)
		}
		if ae.Reason() != c.reason {
			t.Errorf("%s: reason = %q, want %q", c.name, ae.Reason(), c.reason)
		}
	}
}
