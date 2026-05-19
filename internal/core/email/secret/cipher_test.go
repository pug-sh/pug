package secret_test

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/pug-sh/pug/internal/core/email/secret"
)

func newTestKey(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func TestCipherRoundTrip(t *testing.T) {
	c, err := secret.NewCipher(newTestKey(t))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}

	plaintext := []byte(`{"api_key":"sk_test_1234567890"}`)
	blob, err := c.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	got, err := c.Decrypt(blob)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if string(got) != string(plaintext) {
		t.Fatalf("round-trip mismatch: got %q, want %q", got, plaintext)
	}
}

func TestCipherEncryptUsesFreshNonce(t *testing.T) {
	c, err := secret.NewCipher(newTestKey(t))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	a, err := c.Encrypt([]byte("payload"))
	if err != nil {
		t.Fatalf("Encrypt a: %v", err)
	}
	b, err := c.Encrypt([]byte("payload"))
	if err != nil {
		t.Fatalf("Encrypt b: %v", err)
	}
	if string(a) == string(b) {
		t.Fatal("expected different ciphertexts (fresh nonce)")
	}
}

func TestCipherDecryptTamperedFails(t *testing.T) {
	c, err := secret.NewCipher(newTestKey(t))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	blob, err := c.Encrypt([]byte("payload"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	blob[len(blob)-1] ^= 0x01 // flip a bit in the auth tag
	if _, err := c.Decrypt(blob); err == nil {
		t.Fatal("expected decrypt to fail on tampered ciphertext")
	}
}

func TestNewCipherRejectsMalformedKey(t *testing.T) {
	cases := []struct {
		name string
		key  string
	}{
		{"empty", ""},
		{"not_base64", "@@@not-base64@@@"},
		{"wrong_length", base64.StdEncoding.EncodeToString([]byte("too short"))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := secret.NewCipher(tc.key); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			} else if tc.name == "empty" && !strings.Contains(err.Error(), "key") {
				t.Fatalf("expected mention of key in error, got %v", err)
			}
		})
	}
}
