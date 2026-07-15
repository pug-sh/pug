package projects

import (
	"strings"
	"testing"

	"buf.build/go/protovalidate"
	"google.golang.org/protobuf/proto"

	projectsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/projects/v1"
)

// These pin the partial-update validation contract on UpdateMetaRequest. In edition
// 2023, a field with explicit presence skips its rules when unset, so an omitted
// display_name is valid ("leave unchanged"); a present empty string is rejected by
// min_len. If a future protovalidate enforced rules on unset present-able fields,
// the "omitted is valid" cases fail loudly and we'd switch to a message-level CEL.

func TestUpdateMetaRequest_OmittedDisplayNameIsValid(t *testing.T) {
	req := &projectsv1.UpdateMetaRequest{ReportingTimezone: proto.String("Asia/Kolkata")}
	if err := protovalidate.Validate(req); err != nil {
		t.Fatalf("omitted display_name should be valid (partial update): %v", err)
	}
}

func TestUpdateMetaRequest_EmptyDisplayNameRejected(t *testing.T) {
	req := &projectsv1.UpdateMetaRequest{DisplayName: proto.String("")}
	if err := protovalidate.Validate(req); err == nil {
		t.Fatal("present empty display_name should fail min_len")
	}
}

func TestUpdateMetaRequest_OmittedTimezoneIsValid(t *testing.T) {
	req := &projectsv1.UpdateMetaRequest{DisplayName: proto.String("ok")}
	if err := protovalidate.Validate(req); err != nil {
		t.Fatalf("omitted timezone should be valid: %v", err)
	}
}

func TestUpdateMetaRequest_FullyEmptyIsValid(t *testing.T) {
	if err := protovalidate.Validate(&projectsv1.UpdateMetaRequest{}); err != nil {
		t.Fatalf("empty request should be valid (no-op partial update): %v", err)
	}
}

// ----- API key requests -----

// kind is the one field a client cannot get wrong quietly: an unset or unknown
// value would otherwise reach the handler, which fails it closed as an internal
// error. protovalidate turns both into a clean InvalidArgument at the boundary.
func TestCreateApiKeyRequest_KindRequired(t *testing.T) {
	tests := []struct {
		name string
		req  *projectsv1.CreateApiKeyRequest
		ok   bool
	}{
		{"public is valid", &projectsv1.CreateApiKeyRequest{Kind: projectsv1.ApiKeyKind_API_KEY_KIND_PUBLIC.Enum()}, true},
		{"private is valid", &projectsv1.CreateApiKeyRequest{Kind: projectsv1.ApiKeyKind_API_KEY_KIND_PRIVATE.Enum()}, true},
		{"omitted kind rejected", &projectsv1.CreateApiKeyRequest{}, false},
		{"unspecified kind rejected", &projectsv1.CreateApiKeyRequest{Kind: projectsv1.ApiKeyKind_API_KEY_KIND_UNSPECIFIED.Enum()}, false},
		{"undefined kind rejected", &projectsv1.CreateApiKeyRequest{Kind: projectsv1.ApiKeyKind(99).Enum()}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := protovalidate.Validate(tt.req)
			if tt.ok && err != nil {
				t.Fatalf("expected valid, got %v", err)
			}
			if !tt.ok && err == nil {
				t.Fatal("expected a validation error, got nil")
			}
		})
	}
}

func TestCreateApiKeyRequest_DisplayNameBounds(t *testing.T) {
	req := &projectsv1.CreateApiKeyRequest{
		Kind:        projectsv1.ApiKeyKind_API_KEY_KIND_PRIVATE.Enum(),
		DisplayName: proto.String(strings.Repeat("a", 151)),
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Fatal("display_name over 150 chars should fail max_len")
	}

	// A label is optional — an unnamed key is fine.
	req.DisplayName = nil
	if err := protovalidate.Validate(req); err != nil {
		t.Fatalf("omitted display_name should be valid: %v", err)
	}
}

func TestDeleteApiKeyRequest_IDRequired(t *testing.T) {
	if err := protovalidate.Validate(&projectsv1.DeleteApiKeyRequest{}); err == nil {
		t.Fatal("omitted id should fail required")
	}
	if err := protovalidate.Validate(&projectsv1.DeleteApiKeyRequest{Id: proto.String("key1")}); err != nil {
		t.Fatalf("present id should be valid: %v", err)
	}
}
