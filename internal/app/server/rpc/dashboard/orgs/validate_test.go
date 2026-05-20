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

// CreateRequest: display_name has required + min_len=1 + max_len=150.
// Edition 2023 "required" only checks wire presence, so an explicit min_len=1
// is needed to reject the empty string — pinning that rule keeps a future
// removal of min_len from silently regressing empty-name acceptance.

func TestCreateRequest_DisplayNameRequired(t *testing.T) {
	req := &orgsv1.CreateRequest{}
	if err := protovalidate.Validate(req); err == nil {
		t.Error("expected validation error for missing display_name, got nil")
	}
}

func TestCreateRequest_DisplayNameRejectsEmpty(t *testing.T) {
	req := &orgsv1.CreateRequest{DisplayName: proto.String("")}
	if err := protovalidate.Validate(req); err == nil {
		t.Error("expected validation error for empty display_name, got nil")
	}
}

func TestCreateRequest_Valid(t *testing.T) {
	req := &orgsv1.CreateRequest{DisplayName: proto.String("acme")}
	if err := protovalidate.Validate(req); err != nil {
		t.Errorf("expected valid, got error: %v", err)
	}
}

// UpdateDisplayNameRequest: same display_name rules as CreateRequest; pinning
// min_len here too because edition 2023 "required" alone would accept "".

func TestUpdateDisplayNameRequest_DisplayNameRejectsEmpty(t *testing.T) {
	req := &orgsv1.UpdateDisplayNameRequest{
		OrgId:       proto.String("org-1"),
		DisplayName: proto.String(""),
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Error("expected validation error for empty display_name, got nil")
	}
}

// LeaveRequest: org_id required.

func TestLeaveRequest_OrgIDRequired(t *testing.T) {
	req := &orgsv1.LeaveRequest{}
	if err := protovalidate.Validate(req); err == nil {
		t.Error("expected validation error for missing org_id, got nil")
	}
}

func TestLeaveRequest_Valid(t *testing.T) {
	req := &orgsv1.LeaveRequest{OrgId: proto.String("org-1")}
	if err := protovalidate.Validate(req); err != nil {
		t.Errorf("expected valid, got error: %v", err)
	}
}

// UpdateMemberRoleRequest: org_id/customer_id required, role enum required +
// defined_only + not_in:[0]. The "not_in" rule rejects UNSPECIFIED; the
// defined_only rejects unknown enum values.

func TestUpdateMemberRoleRequest_OrgIDRequired(t *testing.T) {
	req := &orgsv1.UpdateMemberRoleRequest{
		CustomerId: proto.String("cust-1"),
		Role:       orgsv1.OrgRole_ORG_ROLE_ADMIN.Enum(),
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Error("expected validation error for missing org_id, got nil")
	}
}

func TestUpdateMemberRoleRequest_CustomerIDRequired(t *testing.T) {
	req := &orgsv1.UpdateMemberRoleRequest{
		OrgId: proto.String("org-1"),
		Role:  orgsv1.OrgRole_ORG_ROLE_ADMIN.Enum(),
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Error("expected validation error for missing customer_id, got nil")
	}
}

func TestUpdateMemberRoleRequest_RoleRejectsUnspecified(t *testing.T) {
	req := &orgsv1.UpdateMemberRoleRequest{
		OrgId:      proto.String("org-1"),
		CustomerId: proto.String("cust-1"),
		Role:       orgsv1.OrgRole_ORG_ROLE_UNSPECIFIED.Enum(),
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Error("expected validation error for UNSPECIFIED role, got nil")
	}
}

func TestUpdateMemberRoleRequest_Valid(t *testing.T) {
	req := &orgsv1.UpdateMemberRoleRequest{
		OrgId:      proto.String("org-1"),
		CustomerId: proto.String("cust-1"),
		Role:       orgsv1.OrgRole_ORG_ROLE_ADMIN.Enum(),
	}
	if err := protovalidate.Validate(req); err != nil {
		t.Errorf("expected valid, got error: %v", err)
	}
}
