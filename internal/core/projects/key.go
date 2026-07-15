package projects

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// Kind distinguishes the two flavors of API key. The values are what the kind
// column stores (see the api_keys_kind_check constraint).
type Kind string

const (
	// KindPublic is embedded in client apps to send events. It is extractable by
	// anyone who ships it, so it is not a secret and is stored plaintext.
	KindPublic Kind = "public"
	// KindPrivate authenticates server-side and MCP callers against the whole
	// project. It is a secret: only its digest is stored, and it is shown to the
	// caller exactly once, at creation.
	KindPrivate Kind = "private"
)

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// NewPrivateKey generates a 24-char private API key: "prv_" + 20 hex chars (80 bits of entropy).
func NewPrivateKey() (string, error) {
	h, err := randomHex(10)
	if err != nil {
		return "", err
	}
	return "prv_" + h, nil
}

// NewPublicKey generates a 24-char public API key: "pub_" + 20 hex chars (80 bits of entropy).
func NewPublicKey() (string, error) {
	h, err := randomHex(10)
	if err != nil {
		return "", err
	}
	return "pub_" + h, nil
}

// hashKey returns the sha256 hex digest a private key is stored and looked up
// by, mirroring coreauth.hashToken. Migration 017 backfills the same digest with
// encode(sha256(convert_to(key, 'UTF8')), 'hex') — the two must agree, or keys
// that predate the api_keys table stop authenticating.
func hashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// mintKey generates a key of the given kind together with the token it is stored
// and looked up by. A public key is not a secret and is kept whole, so the
// settings page can show it again; a private key survives only as its digest.
//
// Minting and the token rule are one switch on purpose. Split apart, a kind could
// be mintable while falling through to "store it whole" — which for a secret means
// storing plaintext — so the pairing makes that state unrepresentable rather than
// merely absent. Fails closed on an unrecognized kind rather than defaulting to a
// flavor the caller did not ask for.
func mintKey(kind Kind) (raw, token string, err error) {
	switch kind {
	case KindPublic:
		raw, err = NewPublicKey()
		if err != nil {
			return "", "", err
		}
		return raw, raw, nil
	case KindPrivate:
		raw, err = NewPrivateKey()
		if err != nil {
			return "", "", err
		}
		return raw, hashKey(raw), nil
	default:
		return "", "", fmt.Errorf("unknown api key kind %q", kind)
	}
}

// maskKeyDisplay renders the hint stored alongside a key ("prv_...3f9c"). It is
// all we can ever show of a private key again, so it has to carry enough to tell
// two of a project's keys apart. A key too short to mask that way collapses to
// "***" (as rpc.maskKey does): masked is a stored, viewer-readable column, so the
// one thing it must never do is echo a secret back whole.
func maskKeyDisplay(key string) string {
	if len(key) <= 8 {
		return "***"
	}
	return key[:4] + "..." + key[len(key)-4:]
}
