package oauth_test

import (
	"errors"
	"strings"
	"testing"

	coreoauth "github.com/pug-sh/pug/internal/core/auth/oauth"
)

func TestNewVerifiedIdentity_Accepts(t *testing.T) {
	id, err := coreoauth.NewVerifiedIdentity(coreoauth.Claims{
		Subject: "sub", Email: "a@b.com", EmailVerified: true, DisplayName: "Name", PictureURI: "uri",
	})
	if err != nil {
		t.Fatalf("NewVerifiedIdentity: %v", err)
	}
	if id.Subject() != "sub" || id.Email() != "a@b.com" || id.DisplayName() != "Name" || id.PictureURI() != "uri" {
		t.Fatalf("accessors mismatch: %q %q %q %q", id.Subject(), id.Email(), id.DisplayName(), id.PictureURI())
	}
}

func TestNewVerifiedIdentity_RejectsUnverified(t *testing.T) {
	if _, err := coreoauth.NewVerifiedIdentity(coreoauth.Claims{Email: "a@b.com", EmailVerified: false}); !errors.Is(err, coreoauth.ErrUnverifiedEmail) {
		t.Fatalf("err = %v, want ErrUnverifiedEmail", err)
	}
}

func TestNewVerifiedIdentity_RejectsEmptyEmail(t *testing.T) {
	if _, err := coreoauth.NewVerifiedIdentity(coreoauth.Claims{Email: "   ", EmailVerified: true}); !errors.Is(err, coreoauth.ErrUnverifiedEmail) {
		t.Fatalf("err = %v, want ErrUnverifiedEmail for blank email", err)
	}
}

// TestNewVerifiedIdentity_ClampsDisplayFields pins that oversized IdP claims are
// truncated (by rune, not byte) to the storage column widths so new-account
// creation can't fail with CodeInternal on an over-long name/picture.
func TestNewVerifiedIdentity_ClampsDisplayFields(t *testing.T) {
	long := strings.Repeat("é", 500) // multi-byte runes exercise rune-safe truncation
	id, err := coreoauth.NewVerifiedIdentity(coreoauth.Claims{
		Subject: "s", Email: "a@b.com", EmailVerified: true, DisplayName: long, PictureURI: long,
	})
	if err != nil {
		t.Fatalf("NewVerifiedIdentity: %v", err)
	}
	if got := len([]rune(id.DisplayName())); got != 150 {
		t.Errorf("display_name runes = %d, want 150", got)
	}
	if got := len([]rune(id.PictureURI())); got != 255 {
		t.Errorf("picture_uri runes = %d, want 255", got)
	}
}
