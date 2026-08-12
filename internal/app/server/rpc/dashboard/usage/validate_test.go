package usage

import (
	"testing"
	"time"

	"buf.build/go/protovalidate"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	usagev1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/usage/v1"
)

// Edition 2023 skips rules on an unset field, so an omitted org_id validates and
// is instead denied by the OrgGated interceptor resolving "" to no membership.
func TestGetUsageRequest_OrgIDMinLenOnlyAppliesWhenPresent(t *testing.T) {
	if err := protovalidate.Validate(&usagev1.GetUsageRequest{}); err != nil {
		t.Fatalf("omitted org_id should be valid: %v", err)
	}
	if err := protovalidate.Validate(&usagev1.GetUsageRequest{OrgId: proto.String("")}); err == nil {
		t.Fatal("present empty org_id should fail min_len")
	}
	if err := protovalidate.Validate(&usagev1.GetUsageRequest{OrgId: proto.String("org1")}); err != nil {
		t.Fatalf("present org_id should be valid: %v", err)
	}
}

// The cap is the only bound on how much of usage_daily one request scans.
func TestGetUsageRequest_RangeSpanCap(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		span time.Duration
		ok   bool
	}{
		{"400 days is accepted", 400 * 24 * time.Hour, true},
		{"just over 400 days is rejected", 400*24*time.Hour + time.Second, false},
		{"a single day is accepted", 24 * time.Hour, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &usagev1.GetUsageRequest{
				OrgId: proto.String("org1"),
				Range: &commonv1.TimeRange{
					From: timestamppb.New(from),
					To:   timestamppb.New(from.Add(tt.span)),
				},
			}
			err := protovalidate.Validate(req)
			if tt.ok && err != nil {
				t.Fatalf("expected valid, got %v", err)
			}
			if !tt.ok && err == nil {
				t.Fatal("expected a validation error, got nil")
			}
		})
	}
}

// An omitted range means "the current period"; a `has(this.range)` slip in the
// message-level CEL would make it mandatory.
func TestGetUsageRequest_OmittedRangeIsValid(t *testing.T) {
	if err := protovalidate.Validate(&usagev1.GetUsageRequest{OrgId: proto.String("org1")}); err != nil {
		t.Fatalf("omitted range should be valid: %v", err)
	}
}

// The handler reads from/to with no ordering check of its own.
func TestGetUsageRequest_InvertedRangeRejected(t *testing.T) {
	from := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	req := &usagev1.GetUsageRequest{
		OrgId: proto.String("org1"),
		Range: &commonv1.TimeRange{
			From: timestamppb.New(from),
			To:   timestamppb.New(from.Add(-24 * time.Hour)),
		},
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Fatal("to before from should fail time_range.from_before_to")
	}
}
