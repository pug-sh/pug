package orgs_test

import (
	"testing"

	"buf.build/go/protovalidate"
	"google.golang.org/protobuf/proto"

	orgsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/orgs/v1"
)

func TestInviteMemberRequest_EmailRequired(t *testing.T) {
	req := &orgsv1.InviteMemberRequest{
		// email intentionally omitted
		OrgId: proto.String("org-1"),
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Error("expected validation error for missing email, got nil")
	}
}

func TestInviteMemberRequest_Valid(t *testing.T) {
	req := &orgsv1.InviteMemberRequest{
		Email: proto.String("test@example.com"),
		OrgId: proto.String("org-1"),
	}
	if err := protovalidate.Validate(req); err != nil {
		t.Errorf("expected valid, got error: %v", err)
	}
}
