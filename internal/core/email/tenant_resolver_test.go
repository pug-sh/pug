package email_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	coreemail "github.com/pug-sh/pug/internal/core/email"
	"github.com/pug-sh/pug/internal/core/email/secret"
)

type recordingProvider struct {
	calls int
}

func (p *recordingProvider) Send(_ context.Context, _ coreemail.Message) error {
	p.calls++
	return nil
}

type stubRepo struct {
	entry coreemail.CachedProviderEntry
	err   error
}

func (r *stubRepo) Get(_ context.Context, _ string) (coreemail.CachedProviderEntry, error) {
	return r.entry, r.err
}

func TestTenantAwareResolverFallbackForNilTenant(t *testing.T) {
	fallback := &recordingProvider{}
	c, _ := secret.NewCipher(testKey32(t))
	r, _ := coreemail.NewTenantAwareResolver(&stubRepo{}, c, fallback, "ops@operator.com", "reply@operator.com")

	provider, from, err := r.Resolve(context.Background(), nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if provider != fallback {
		t.Fatal("expected fallback provider for nil tenant")
	}
	if from.From != "ops@operator.com" || from.ReplyTo != "reply@operator.com" {
		t.Fatalf("expected operator defaults, got %+v", from)
	}
}

func TestTenantAwareResolverFallbackOnAbsent(t *testing.T) {
	fallback := &recordingProvider{}
	c, _ := secret.NewCipher(testKey32(t))
	r, _ := coreemail.NewTenantAwareResolver(&stubRepo{entry: coreemail.CachedProviderEntry{Present: false}}, c, fallback, "ops@operator.com", "")

	tenant := "org-x"
	provider, from, err := r.Resolve(context.Background(), &tenant)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if provider != fallback {
		t.Fatal("expected fallback when entry absent")
	}
	if from.From != "ops@operator.com" {
		t.Fatalf("expected operator From on absent, got %q", from.From)
	}
}

func TestTenantAwareResolverDecryptsAndBuildsResend(t *testing.T) {
	c, _ := secret.NewCipher(testKey32(t))
	plain, _ := coreemail.EncodeProviderConfig(coreemail.ProviderKindResend, coreemail.ResendConfig{APIKey: "sk_test_x"})
	blob, _ := c.Encrypt(plain)

	entry := coreemail.CachedProviderEntry{
		Present:          true,
		Kind:             string(coreemail.ProviderKindResend),
		FromAddress:      "ops@acme.com",
		ReplyTo:          "support@acme.com",
		SecretCiphertext: blob,
	}
	fallback := &recordingProvider{}
	r, _ := coreemail.NewTenantAwareResolver(&stubRepo{entry: entry}, c, fallback, "ops@operator.com", "")

	tenant := "org-x"
	provider, from, err := r.Resolve(context.Background(), &tenant)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if provider == fallback {
		t.Fatal("expected per-tenant provider, got fallback")
	}
	if from.From != "ops@acme.com" || from.ReplyTo != "support@acme.com" {
		t.Fatalf("expected tenant From/ReplyTo, got %+v", from)
	}
	// Sanity-check the encoded plaintext round-trips.
	var verify coreemail.ResendConfig
	if err := json.Unmarshal(plain, &verify); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if verify.APIKey != "sk_test_x" {
		t.Fatalf("config roundtrip mismatch: %+v", verify)
	}
}

func TestTenantAwareResolverPropagatesRepoError(t *testing.T) {
	fallback := &recordingProvider{}
	c, _ := secret.NewCipher(testKey32(t))
	wantErr := errors.New("db blew up")
	r, _ := coreemail.NewTenantAwareResolver(&stubRepo{err: wantErr}, c, fallback, "ops@operator.com", "")

	tenant := "org-y"
	_, _, err := r.Resolve(context.Background(), &tenant)
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected repo error to propagate, got %v", err)
	}
}

// perTenantRepo returns a different CachedProviderEntry per orgID and records
// the orgIDs it was called with, so cross-tenant isolation can be asserted.
type perTenantRepo struct {
	entries map[string]coreemail.CachedProviderEntry
	calls   []string
}

func (r *perTenantRepo) Get(_ context.Context, orgID string) (coreemail.CachedProviderEntry, error) {
	r.calls = append(r.calls, orgID)
	if e, ok := r.entries[orgID]; ok {
		return e, nil
	}
	return coreemail.CachedProviderEntry{Present: false}, nil
}

// TestTenantAwareResolverIsolatesPerTenant pins cross-tenant isolation: two
// orgs with different per-tenant configs must each resolve to their own
// FromAddress, and the repo must be queried with each orgID (not a cached
// or hard-coded one). A regression that captured a tenantID in a closure or
// memoized the first entry would fail this test.
func TestTenantAwareResolverIsolatesPerTenant(t *testing.T) {
	c, _ := secret.NewCipher(testKey32(t))
	plainA, _ := coreemail.EncodeProviderConfig(coreemail.ProviderKindResend, coreemail.ResendConfig{APIKey: "sk_a"})
	plainB, _ := coreemail.EncodeProviderConfig(coreemail.ProviderKindResend, coreemail.ResendConfig{APIKey: "sk_b"})
	blobA, _ := c.Encrypt(plainA)
	blobB, _ := c.Encrypt(plainB)

	repo := &perTenantRepo{
		entries: map[string]coreemail.CachedProviderEntry{
			"org-a": {Present: true, Kind: string(coreemail.ProviderKindResend), FromAddress: "ops@a.com", ReplyTo: "rt@a.com", SecretCiphertext: blobA},
			"org-b": {Present: true, Kind: string(coreemail.ProviderKindResend), FromAddress: "ops@b.com", ReplyTo: "rt@b.com", SecretCiphertext: blobB},
		},
	}
	fallback := &recordingProvider{}
	r, _ := coreemail.NewTenantAwareResolver(repo, c, fallback, "ops@operator.com", "")

	tenantA := "org-a"
	_, fromA, err := r.Resolve(context.Background(), &tenantA)
	if err != nil {
		t.Fatalf("Resolve org-a: %v", err)
	}
	tenantB := "org-b"
	_, fromB, err := r.Resolve(context.Background(), &tenantB)
	if err != nil {
		t.Fatalf("Resolve org-b: %v", err)
	}
	if fromA.From != "ops@a.com" || fromA.ReplyTo != "rt@a.com" {
		t.Fatalf("org-a got %+v, expected ops@a.com / rt@a.com", fromA)
	}
	if fromB.From != "ops@b.com" || fromB.ReplyTo != "rt@b.com" {
		t.Fatalf("org-b got %+v, expected ops@b.com / rt@b.com", fromB)
	}
	if len(repo.calls) != 2 || repo.calls[0] != "org-a" || repo.calls[1] != "org-b" {
		t.Fatalf("expected repo calls [org-a, org-b], got %v", repo.calls)
	}
}

// TestTenantAwareResolverDecryptFailureIsPermanent pins the DLQ-vs-infinite-
// retry decision point. If the operator rotates PUG_EMAIL_PROVIDER_SECRET_KEY
// without re-encrypting rows, retrying the same row will keep failing — the
// worker must DLQ rather than spin. A regression that dropped the
// NewPermanentError wrap would cause every email to a misconfigured tenant
// to retry until MaxDeliver.
func TestTenantAwareResolverDecryptFailureIsPermanent(t *testing.T) {
	enc, _ := secret.NewCipher(testKey32(t))
	// Build a *different* key so dec cannot decrypt enc's blob.
	const otherKey = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBA="
	dec, _ := secret.NewCipher(otherKey)

	plain, _ := coreemail.EncodeProviderConfig(coreemail.ProviderKindResend, coreemail.ResendConfig{APIKey: "sk_x"})
	blob, _ := enc.Encrypt(plain)

	entry := coreemail.CachedProviderEntry{
		Present:          true,
		Kind:             string(coreemail.ProviderKindResend),
		FromAddress:      "ops@acme.com",
		SecretCiphertext: blob,
	}
	r, _ := coreemail.NewTenantAwareResolver(&stubRepo{entry: entry}, dec, &recordingProvider{}, "ops@operator.com", "")

	tenant := "org-x"
	_, _, err := r.Resolve(context.Background(), &tenant)
	if err == nil {
		t.Fatal("expected Resolve to fail when ciphertext was encrypted with a different key")
	}
	if !coreemail.IsPermanentError(err) {
		t.Fatalf("expected permanent error so worker DLQs, got %v", err)
	}
}

// TestTenantAwareResolverUnknownKindIsPermanent pins the DB-CHECK-vs-Go-
// constants drift guard. If a row lands with a Kind that buildProvider does
// not handle (e.g. a future PROVIDER_KIND_MAILGUN added to proto/migration
// but not wired into the switch), the resolver must DLQ rather than NAK.
func TestTenantAwareResolverUnknownKindIsPermanent(t *testing.T) {
	c, _ := secret.NewCipher(testKey32(t))
	blob, _ := c.Encrypt([]byte(`{"any":"shape"}`))
	entry := coreemail.CachedProviderEntry{
		Present:          true,
		Kind:             "ORG_EMAIL_PROVIDER_KIND_MAILGUN",
		FromAddress:      "ops@acme.com",
		SecretCiphertext: blob,
	}
	r, _ := coreemail.NewTenantAwareResolver(&stubRepo{entry: entry}, c, &recordingProvider{}, "ops@operator.com", "")

	tenant := "org-x"
	_, _, err := r.Resolve(context.Background(), &tenant)
	if err == nil {
		t.Fatal("expected Resolve to fail on unknown provider kind")
	}
	if !coreemail.IsPermanentError(err) {
		t.Fatalf("expected permanent error on unknown kind, got %v", err)
	}
}

func testKey32(t *testing.T) string {
	t.Helper()
	const k = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	return k
}
