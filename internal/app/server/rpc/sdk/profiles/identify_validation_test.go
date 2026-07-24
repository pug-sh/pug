package profiles

import (
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
// anonymous_id (^$|^anon-) and BatchCreateRequest.distinct_id
// (batch.distinct_id_reserved_prefix) were both guarded; external_id carried only
// `required`, while ingestion.md claims the prefix is reserved against client-sent
// ids outright. Two of three is not a reserved namespace.
//
// The concrete failure it allows: a tenant whose external_id is user-supplied (a
// username, not an email) lets someone register as "cookieless-x". Identify
// succeeds, and post-identify events are keyed by external_id — so every later
// BatchCreate carrying that user trips the reserved-prefix rule. Because that CEL
// is `this.events.all(...)`, the WHOLE batch is rejected, taking unrelated users'
// events down with it on a server-side SDK.
func TestIdentify_RejectsCookielessExternalID(t *testing.T) {
	req := &sdkprofilesv1.IdentifyRequest{
		ExternalId: proto.String(cookieless.IDPrefix + "3q28pTkXhWZbA4NUJ9wA"),
	}
	err := protovalidate.Validate(req)
	if err == nil {
		t.Fatal("cookieless- external_id must fail validation — the prefix is server-reserved")
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
