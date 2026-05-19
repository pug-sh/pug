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
	r := coreemail.NewTenantAwareResolver(&stubRepo{}, c, fallback, "ops@operator.com", "reply@operator.com")

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
	r := coreemail.NewTenantAwareResolver(&stubRepo{entry: coreemail.CachedProviderEntry{Present: false}}, c, fallback, "ops@operator.com", "")

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
	r := coreemail.NewTenantAwareResolver(&stubRepo{entry: entry}, c, fallback, "ops@operator.com", "")

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
	r := coreemail.NewTenantAwareResolver(&stubRepo{err: wantErr}, c, fallback, "ops@operator.com", "")

	tenant := "org-y"
	_, _, err := r.Resolve(context.Background(), &tenant)
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected repo error to propagate, got %v", err)
	}
}

func testKey32(t *testing.T) string {
	t.Helper()
	const k = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	return k
}
