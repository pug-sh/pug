package apperr

import (
	"testing"

	"connectrpc.com/connect"
)

func TestConstructors_setCodeAndReason(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code connect.Code
	}{
		{"NotFound", NotFound(ReasonProfileNotFound, "m"), connect.CodeNotFound},
		{"Invalid", Invalid(ReasonProfileNotFound, "m"), connect.CodeInvalidArgument},
		{"AlreadyExists", AlreadyExists(ReasonProfileNotFound, "m"), connect.CodeAlreadyExists},
		{"PermissionDenied", PermissionDenied(ReasonProfileNotFound, "m"), connect.CodePermissionDenied},
		{"FailedPrecondition", FailedPrecondition(ReasonProfileNotFound, "m"), connect.CodeFailedPrecondition},
		{"Unauthenticated", Unauthenticated(ReasonUnauthenticated, "m"), connect.CodeUnauthenticated},
	}
	for _, c := range cases {
		ae := c.err.(*Error)
		if ae.Code != c.code {
			t.Errorf("%s: code = %v, want %v", c.name, ae.Code, c.code)
		}
	}
}
