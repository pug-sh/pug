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
