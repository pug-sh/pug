package profiles

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"buf.build/go/protovalidate"
	"google.golang.org/protobuf/proto"

	"github.com/pug-sh/pug/internal/cookieless"
	sdkprofilesv1 "github.com/pug-sh/pug/internal/gen/proto/sdk/profiles/v1"
)

// The anonymous_id pattern ^$|^anon- is what makes identify-from-cookieless
// structurally impossible: a cookieless- id cannot match it, so NAT-shared
// cookieless hashes can never be merged into a real profile. This pin exists
// so a future relaxation of that pattern cannot silently open the door.
func TestIdentify_RejectsCookielessAnonymousID(t *testing.T) {
	req := &sdkprofilesv1.IdentifyRequest{
		ExternalId:  proto.String("user@example.com"),
		AnonymousId: proto.String(cookieless.IDPrefix + "3q28pTkXhWZbA4NUJ9wA"),
	}
	err := protovalidate.Validate(req)
	if err == nil {
		t.Fatal("cookieless- anonymous_id must fail validation")
	}
	if !strings.Contains(err.Error(), "anonymous_id") {
		t.Errorf("violation should name anonymous_id, got: %v", err)
	}

	ok := &sdkprofilesv1.IdentifyRequest{
		ExternalId:  proto.String("user@example.com"),
		AnonymousId: proto.String("anon-1234"),
	}
	if err := protovalidate.Validate(ok); err != nil {
		t.Errorf("anon- id must remain valid: %v", err)
	}
}

// TestIdentify_RejectsCookielessExternalID closes the third id field.
//
// anonymous_id (^$|^anon-) and an incoming event's distinct_id
// (incoming_event.distinct_id_reserved_prefix) were both guarded; external_id carried only
// `required`, while ingestion.md claims the prefix is reserved against client-sent
// ids outright. Two of three is not a reserved namespace.
//
// The concrete failure it allows: a tenant whose external_id is user-supplied (a
// username, not an email) lets someone register as "cookieless-x". Identify
// succeeds, and post-identify events are keyed by external_id, so BatchCreate
// drops every later event of that user on the reserved-prefix rule.
func TestIdentify_RejectsCookielessExternalID(t *testing.T) {
	req := &sdkprofilesv1.IdentifyRequest{
		ExternalId: proto.String(cookieless.IDPrefix + "3q28pTkXhWZbA4NUJ9wA"),
	}
	err := protovalidate.Validate(req)
	if err == nil {
		t.Fatal("cookieless- external_id must fail validation: the prefix is server-reserved")
	}
	if !strings.Contains(err.Error(), "external_id") {
		t.Errorf("violation should name external_id, got: %v", err)
	}

	// The guard must reject only the reserved prefix, not ordinary ids that
	// happen to contain the substring elsewhere.
	for _, id := range []string{"user@example.com", "my-cookieless-account", "anon-1234"} {
		ok := &sdkprofilesv1.IdentifyRequest{ExternalId: proto.String(id)}
		if err := protovalidate.Validate(ok); err != nil {
			t.Errorf("external_id %q must remain valid: %v", id, err)
		}
	}
}

// `required` only checks presence under editions, so an explicit "" needs min_len.
// Without it the identify is accepted, then the worker's own min_len rejects it and
// it dead-letters while the caller was told it succeeded.
func TestIdentify_RejectsEmptyExternalID(t *testing.T) {
	err := protovalidate.Validate(&sdkprofilesv1.IdentifyRequest{ExternalId: proto.String("")})
	if err == nil {
		t.Fatal("empty external_id must fail validation")
	}
	if !strings.Contains(err.Error(), "external_id") {
		t.Errorf("violation should name external_id, got: %v", err)
	}
}

// Postgres text can't hold a NUL, so an id carrying one would be accepted and then
// dead-letter in the worker. The envelope mirrors external_id's rule.
func TestIdentify_RejectsNULInIDs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field string
		msg   proto.Message
	}{
		{"request external_id", "external_id", &sdkprofilesv1.IdentifyRequest{ExternalId: proto.String("user\x001")}},
		{"request anonymous_id", "anonymous_id", &sdkprofilesv1.IdentifyRequest{ExternalId: proto.String("user-1"), AnonymousId: proto.String("anon-\x001")}},
		{"request device_id", "device_id", &sdkprofilesv1.IdentifyRequest{ExternalId: proto.String("user-1"), DeviceId: proto.String("dev\x001")}},
		{"envelope external_id", "external_id", &sdkprofilesv1.ProfileIdentifyMessage{ExternalId: proto.String("user\x001"), ProjectId: proto.String("p1")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := protovalidate.Validate(tc.msg)
			ve, ok := errors.AsType[*protovalidate.ValidationError](err)
			if !ok || len(ve.Violations) != 1 || ve.Violations[0].Proto.GetRuleId() != tc.field+".no_nul" {
				t.Errorf("got %v, want only a %s.no_nul violation", err, tc.field)
			}
		})
	}
}

// The request holds only max_items: BatchIdentify validates each item itself, so an
// item breaking an Identify rule leaves the request valid and is skipped alone.
func TestBatchIdentifyRequest_Validation(t *testing.T) {
	item := func(externalID, anonymousID string) *sdkprofilesv1.IdentifyRequest {
		return &sdkprofilesv1.IdentifyRequest{ExternalId: proto.String(externalID), AnonymousId: proto.String(anonymousID)}
	}
	full := make([]*sdkprofilesv1.IdentifyRequest, 1000)
	for i := range full {
		full[i] = item(fmt.Sprintf("user-%d", i), "")
	}
	for _, tc := range []struct {
		name    string
		items   []*sdkprofilesv1.IdentifyRequest
		wantErr string // "" means valid
	}{
		{"empty", nil, ""},
		{"1000 items", full, ""},
		{"1001 items", append(slices.Clone(full), item("user-x", "")), "identifies"},
		{"cookieless external id", []*sdkprofilesv1.IdentifyRequest{item("user-1", ""), item(cookieless.IDPrefix+"abc", "")}, ""},
		{"empty external id", []*sdkprofilesv1.IdentifyRequest{item("user-1", ""), item("", "")}, ""},
		{"bad anonymous id", []*sdkprofilesv1.IdentifyRequest{item("user-1", "nope")}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := protovalidate.Validate(&sdkprofilesv1.BatchIdentifyRequest{Identifies: tc.items})
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("want valid, got: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Errorf("want a violation naming %s, got none", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("violation should name %s, got: %v", tc.wantErr, err)
			}
		})
	}
}
