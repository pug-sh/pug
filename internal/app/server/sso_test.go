package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"code.dny.dev/ssrf"
)

// With no key, connections in the database are reported at startup: none can sign anyone in.
func TestWarnStrandedSSOConnections(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	for _, found := range []bool{false, true} {
		logs.Reset()
		warnStrandedSSOConnections(t.Context(), func(context.Context) (bool, error) { return found, nil })
		if got := strings.Contains(logs.String(), "PUG_SSO_SECRET_KEY is empty"); got != found {
			t.Errorf("connections = %t: logs = %q", found, logs.String())
		}
	}
}

// Without PUG_SSO_ALLOW_PRIVATE_ISSUERS, a connection's issuer can't be on a private address.
func TestNewSSO(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	t.Setenv("PUG_SSO_SECRET_KEY", "")
	if d, err := newSSO(t.Context()); err != nil || d.cipher != nil {
		t.Fatalf("no key: %+v, %v; want connections off", d, err)
	}
	t.Setenv("PUG_SSO_SECRET_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 16))))
	if _, err := newSSO(t.Context()); err == nil {
		t.Fatal("16-byte key: want a startup error, not connections off")
	}
	t.Setenv("PUG_SSO_SECRET_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))))
	d, err := newSSO(t.Context())
	if err != nil || d.cipher == nil {
		t.Fatalf("newSSO = %+v, %v", d, err)
	}
	if err := d.checkIssuer(t.Context(), srv.URL); !errors.Is(err, ssrf.ErrProhibitedIP) {
		t.Fatalf("checkIssuer(loopback) = %v, want ssrf.ErrProhibitedIP", err)
	}
	t.Setenv("PUG_SSO_ALLOW_PRIVATE_ISSUERS", "true")
	if d, err = newSSO(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := d.checkIssuer(t.Context(), srv.URL); errors.Is(err, ssrf.ErrProhibitedIP) {
		t.Fatalf("checkIssuer(loopback) with private issuers allowed = %v", err)
	}
}
