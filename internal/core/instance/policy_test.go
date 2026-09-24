package instance

import "testing"

func TestParsePolicy(t *testing.T) {
	t.Run("open default", func(t *testing.T) {
		p, err := ParsePolicy("", "")
		if err != nil || p.Managed() || p.Mode() != "open" {
			t.Fatalf("unexpected open policy: %v, %v", p, err)
		}
	})
	t.Run("managed admin address is case insensitive", func(t *testing.T) {
		p, err := ParsePolicy("managed", " Operator@Example.com ")
		if err != nil || !p.Managed() || !p.AllowsAdmin("operator@example.com") {
			t.Fatalf("unexpected managed policy: %v, %v", p, err)
		}
	})
	for _, tc := range []struct{ mode, admins string }{
		{"unknown", "admin@example.com"},
		{"managed", ""},
		{"managed", "not-an-email"},
		{"managed", "admin@example.com,ADMIN@example.com"},
		{"managed", "admin@example.com,"},
	} {
		if _, err := ParsePolicy(tc.mode, tc.admins); err == nil {
			t.Errorf("ParsePolicy(%q, %q) accepted invalid config", tc.mode, tc.admins)
		}
	}
}
