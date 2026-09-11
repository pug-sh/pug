package payments

import (
	"errors"
	"testing"

	"github.com/pug-sh/pug/internal/deps/dodo"
)

func TestNewNoProviderNamed(t *testing.T) {
	t.Setenv("PUG_DODO_API_KEY", "")
	p, err := New(t.Context(), "")
	if err != nil || p != nil {
		t.Fatalf("New = (%v, %v), want (nil, nil)", p, err)
	}
}

func TestNewRejectsAnUnknownProvider(t *testing.T) {
	if _, err := New(t.Context(), "stripe"); err == nil {
		t.Fatal("an unknown provider was accepted")
	}
}

// Both binaries read the same variable through here, so a name one would start on
// must not fail the other.
func TestNewNormalizesTheProviderName(t *testing.T) {
	t.Setenv("PUG_DODO_API_KEY", "sk_test")
	for _, name := range []string{"  ", "  DODO  ", "Dodo", dodo.Name} {
		if _, err := New(t.Context(), name); err != nil {
			t.Errorf("New(%q): %v", name, err)
		}
	}
}

// Reported rather than decided: the server degrades to no buy button, the
// reconcile pass exits non-zero.
func TestNewReportsAMissingAPIKey(t *testing.T) {
	t.Setenv("PUG_DODO_API_KEY", "")
	p, err := New(t.Context(), dodo.Name)
	if !errors.Is(err, ErrNoAPIKey) || p != nil {
		t.Fatalf("New = (%v, %v), want ErrNoAPIKey", p, err)
	}
}

func TestNewCarriesTheMandateProduct(t *testing.T) {
	t.Setenv("PUG_DODO_API_KEY", "sk_test")
	t.Setenv("PUG_DODO_MANDATE_PRODUCT", " prod_mandate ")

	p, err := New(t.Context(), dodo.Name)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p == nil {
		t.Fatal("New returned no provider for a configured deployment")
	}
	if p.MandateProduct != "prod_mandate" {
		t.Errorf("MandateProduct = %q", p.MandateProduct)
	}
	// Only a caller that starts checkouts knows where a buyer returns to.
	if p.ReturnURL != "" {
		t.Errorf("ReturnURL = %q, want empty", p.ReturnURL)
	}
}
