package projects

import (
	"regexp"
	"strings"
	"testing"
)

var hexPattern = regexp.MustCompile(`^[0-9a-f]+$`)

func TestNewPrivateKey(t *testing.T) {
	key := NewPrivateKey()
	if !strings.HasPrefix(key, "prv_") {
		t.Errorf("private key %q does not start with prv_", key)
	}
	if len(key) != 24 {
		t.Errorf("private key length = %d, want 24", len(key))
	}
	hex := key[4:]
	if !hexPattern.MatchString(hex) {
		t.Errorf("private key hex portion %q contains non-hex chars", hex)
	}
}

func TestNewPublicKey(t *testing.T) {
	key := NewPublicKey()
	if !strings.HasPrefix(key, "pub_") {
		t.Errorf("public key %q does not start with pub_", key)
	}
	if len(key) != 24 {
		t.Errorf("public key length = %d, want 24", len(key))
	}
	hex := key[4:]
	if !hexPattern.MatchString(hex) {
		t.Errorf("public key hex portion %q contains non-hex chars", hex)
	}
}

func TestKeyUniqueness(t *testing.T) {
	const n = 100
	seen := make(map[string]struct{}, n*2)

	for i := 0; i < n; i++ {
		prv := NewPrivateKey()
		pub := NewPublicKey()
		if _, exists := seen[prv]; exists {
			t.Fatalf("duplicate private key on iteration %d: %s", i, prv)
		}
		if _, exists := seen[pub]; exists {
			t.Fatalf("duplicate public key on iteration %d: %s", i, pub)
		}
		seen[prv] = struct{}{}
		seen[pub] = struct{}{}
	}
}

func TestKeysDiffer(t *testing.T) {
	a := NewPrivateKey()
	b := NewPrivateKey()
	if a == b {
		t.Errorf("two consecutive private keys are identical: %s", a)
	}
	c := NewPublicKey()
	d := NewPublicKey()
	if c == d {
		t.Errorf("two consecutive public keys are identical: %s", c)
	}
}
