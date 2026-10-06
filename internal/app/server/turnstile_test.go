package server

import "testing"

func TestNewTurnstile(t *testing.T) {
	for _, tc := range []struct {
		site, secret string
		on, fails    bool
	}{
		{},
		{site: "site", secret: "secret", on: true},
		{site: "site", fails: true},
		{secret: "secret", fails: true},
		{site: " ", secret: "secret", fails: true},
	} {
		t.Setenv("PUG_TURNSTILE_SITE_KEY", tc.site)
		t.Setenv("PUG_TURNSTILE_SECRET_KEY", tc.secret)
		v, err := newTurnstile(t.Context())
		if (err != nil) != tc.fails || (v != nil) != tc.on || tc.on && v.SiteKey() != tc.site {
			t.Errorf("site %q, secret %q: newTurnstile = %v, %v", tc.site, tc.secret, v, err)
		}
	}
}
