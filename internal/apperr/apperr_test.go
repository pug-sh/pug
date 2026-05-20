package apperr

import (
	"errors"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
)

func assertPanics(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Error("expected panic, got none")
		}
	}()
	fn()
}

func TestReasonRegistry_add(t *testing.T) {
	t.Run("rejects bad format", func(t *testing.T) {
		for _, bad := range []string{"bad-format", "not_found", "TRAILING_", "DOUBLE__US", "_LEADING", "1BAD", ""} {
			r := &reasonRegistry{seen: map[string]struct{}{}}
			assertPanics(t, func() { r.add(bad) })
		}
	})
	t.Run("rejects duplicate", func(t *testing.T) {
		r := &reasonRegistry{seen: map[string]struct{}{}}
		r.add("OK_CODE")
		assertPanics(t, func() { r.add("OK_CODE") })
	})
	t.Run("accepts valid unique", func(t *testing.T) {
		r := &reasonRegistry{seen: map[string]struct{}{}}
		if got := r.add("PROFILE_NOT_FOUND"); got != "PROFILE_NOT_FOUND" {
			t.Errorf("add = %q, want PROFILE_NOT_FOUND", got)
		}
	})
}

func TestReasonForCode_coversConnectCodes(t *testing.T) {
	cs := []connect.Code{
		connect.CodeCanceled, connect.CodeUnknown, connect.CodeInvalidArgument,
		connect.CodeDeadlineExceeded, connect.CodeNotFound, connect.CodeAlreadyExists,
		connect.CodePermissionDenied, connect.CodeResourceExhausted,
		connect.CodeFailedPrecondition, connect.CodeAborted, connect.CodeOutOfRange,
		connect.CodeUnimplemented, connect.CodeInternal, connect.CodeUnavailable,
		connect.CodeDataLoss, connect.CodeUnauthenticated,
	}
	for _, c := range cs {
		r := ReasonForCode(c)
		if r == "" {
			t.Errorf("ReasonForCode(%v) is empty", c)
		}
		if _, ok := codes.seen[r]; !ok {
			t.Errorf("ReasonForCode(%v)=%q not registered", c, r)
		}
	}
}

func TestErr_threadsOptionsIntoDetails(t *testing.T) {
	applied := func(e *Error) { e.details = append(e.details, &errdetails.ResourceInfo{ResourceType: "x"}) }
	err := Err(connect.CodeNotFound, ReasonProfileNotFound, "nope", applied)
	var ae *Error
	if !errors.As(err, &ae) {
		t.Fatalf("want *Error, got %T", err)
	}
	if len(ae.Details()) != 1 {
		t.Fatalf("Details() len = %d, want 1", len(ae.Details()))
	}
}

func TestErr_noOptions_nilDetails(t *testing.T) {
	err := Err(connect.CodeNotFound, ReasonProfileNotFound, "nope")
	var ae *Error
	errors.As(err, &ae)
	if ae.Details() != nil {
		t.Errorf("Details() = %v, want nil", ae.Details())
	}
}

func TestErr_carriesReason(t *testing.T) {
	err := Err(connect.CodeNotFound, ReasonProfileNotFound, "profile not found")
	var ae *Error
	if !errors.As(err, &ae) {
		t.Fatalf("expected *Error, got %T", err)
	}
	if ae.Code != connect.CodeNotFound || ae.Reason != "PROFILE_NOT_FOUND" || ae.Message != "profile not found" {
		t.Errorf("got %+v", ae)
	}
	if ae.Error() != "profile not found" {
		t.Errorf("Error() = %q", ae.Error())
	}
}
