package apperr

import (
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
)

func TestResource(t *testing.T) {
	err := Err(connect.CodeNotFound, ReasonProfileNotFound, "x", Resource("org", "o_123")).(*Error)
	ri, ok := err.details[0].(*errdetails.ResourceInfo)
	if !ok || ri.GetResourceType() != "org" || ri.GetResourceName() != "o_123" {
		t.Fatalf("got %+v", err.details)
	}
}

func TestResource_appendsDistinctDetails(t *testing.T) {
	// Two Resource options describe two different resources, so they must NOT be
	// coalesced (unlike Precondition/Field, which merge into one container). A
	// regression that coalesced them would silently merge two missing-resource
	// reports into one.
	err := Err(connect.CodeNotFound, ReasonProfileNotFound, "x",
		Resource("org", "o_123"), Resource("project", "p_456")).(*Error)
	if len(err.details) != 2 {
		t.Fatalf("want 2 distinct ResourceInfo details (not coalesced), got %d", len(err.details))
	}
	for i, want := range []struct{ typ, name string }{{"org", "o_123"}, {"project", "p_456"}} {
		ri, ok := err.details[i].(*errdetails.ResourceInfo)
		if !ok || ri.GetResourceType() != want.typ || ri.GetResourceName() != want.name {
			t.Fatalf("detail %d = %+v, want %s/%s", i, err.details[i], want.typ, want.name)
		}
	}
}

func TestPrecondition_appendsViolations(t *testing.T) {
	err := Err(connect.CodeFailedPrecondition, ReasonProfileNotFound, "x",
		Precondition("TOS", "user", "must accept"),
		Precondition("TOS", "user", "again")).(*Error)
	if len(err.details) != 1 {
		t.Fatalf("want 1 PreconditionFailure, got %d details", len(err.details))
	}
	pf := err.details[0].(*errdetails.PreconditionFailure)
	if len(pf.GetViolations()) != 2 {
		t.Fatalf("want 2 violations, got %d", len(pf.GetViolations()))
	}
}

func TestField_appendsFieldViolations(t *testing.T) {
	err := Err(connect.CodeInvalidArgument, ReasonProfileNotFound, "x",
		Field("email", "bad"), Field("name", "empty")).(*Error)
	br := err.details[0].(*errdetails.BadRequest)
	if len(br.GetFieldViolations()) != 2 {
		t.Fatalf("want 2 field violations, got %d", len(br.GetFieldViolations()))
	}
}
