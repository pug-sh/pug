package projects

import (
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
