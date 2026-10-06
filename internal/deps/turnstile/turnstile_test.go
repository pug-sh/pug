package turnstile

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testVerifier(t *testing.T, h http.HandlerFunc) *Verifier {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	v := New("site-key", "secret-key")
	v.url = srv.URL
	return v
}

func answer(body string) http.HandlerFunc {
	return answerStatus(http.StatusOK, body)
}

func answerStatus(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func TestVerify(t *testing.T) {
	tests := []struct {
		name     string
		handler  http.HandlerFunc
		ok       bool
		rejected bool
	}{
		{name: "accepted", handler: answer(`{"success":true}`), ok: true},
		{name: "bad token", handler: answer(`{"success":false,"error-codes":["invalid-input-response"]}`), rejected: true},
		{name: "spent token", handler: answer(`{"success":false,"error-codes":["timeout-or-duplicate"]}`), rejected: true},
		{name: "wrong secret", handler: answer(`{"success":false,"error-codes":["invalid-input-secret"]}`)},
		{name: "wrong secret and bad token", handler: answer(`{"success":false,"error-codes":["invalid-input-response","invalid-input-secret"]}`)},
		{name: "cloudflare fault", handler: answer(`{"success":false,"error-codes":["internal-error"]}`)},
		{name: "refused without codes", handler: answer(`{"success":false}`)},
		{name: "bad json", handler: answer(`<html>`)},
		{name: "non-200", handler: answerStatus(http.StatusBadGateway, `{"success":true}`)},
		{name: "wrong secret as a 400", handler: answerStatus(http.StatusBadRequest, `{"success":false,"error-codes":["invalid-input-secret"]}`)},
		{name: "bad token as a 400", handler: answerStatus(http.StatusBadRequest, `{"success":false,"error-codes":["invalid-input-response"]}`), rejected: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := testVerifier(t, tt.handler).Verify(t.Context(), "token")
			if (err == nil) != tt.ok || errors.Is(err, ErrRejected) != tt.rejected {
				t.Fatalf("Verify = %v, want ok %t, rejected %t", err, tt.ok, tt.rejected)
			}
		})
	}
}

func TestVerifySendsOnlySecretAndToken(t *testing.T) {
	v := testVerifier(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.Method != http.MethodPost || len(r.PostForm) != 2 || r.PostForm.Get("secret") != "secret-key" || r.PostForm.Get("response") != "token" {
			t.Errorf("request = %s %v", r.Method, r.PostForm)
		}
		_, _ = io.WriteString(w, `{"success":true}`)
	})
	if err := v.Verify(t.Context(), "token"); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyWithoutTokenSkipsCloudflare(t *testing.T) {
	v := testVerifier(t, func(http.ResponseWriter, *http.Request) { t.Error("siteverify called without a token") })
	if err := v.Verify(t.Context(), ""); !errors.Is(err, ErrRejected) {
		t.Fatalf("Verify = %v, want ErrRejected", err)
	}
}

func TestVerifyTimesOut(t *testing.T) {
	release := make(chan struct{})
	v := testVerifier(t, func(http.ResponseWriter, *http.Request) { <-release })
	t.Cleanup(func() { close(release) })
	v.client.Timeout = 50 * time.Millisecond
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	if err := v.Verify(t.Context(), "token"); err == nil || errors.Is(err, ErrRejected) {
		t.Fatalf("Verify = %v, want a failure that is not ErrRejected", err)
	}
	if !strings.Contains(logs.String(), "turnstile check failed") {
		t.Fatalf("timeout not logged: %q", logs.String())
	}
}

func TestVerifyLogsOnlyCloudflareFailures(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	v := testVerifier(t, answerStatus(http.StatusBadGateway, ""))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := v.Verify(ctx, "token"); err == nil {
		t.Fatal("Verify with a canceled context = nil")
	}
	refuses := testVerifier(t, answer(`{"success":false,"error-codes":["timeout-or-duplicate"]}`))
	if err := refuses.Verify(t.Context(), "token"); !errors.Is(err, ErrRejected) {
		t.Fatalf("Verify = %v, want ErrRejected", err)
	}
	if err := testVerifier(t, answer(`{"success":true}`)).Verify(t.Context(), "token"); err != nil {
		t.Fatal(err)
	}
	if logs.Len() != 0 {
		t.Fatalf("canceled, refused or accepted request logged: %q", logs.String())
	}
	if err := v.Verify(t.Context(), "token"); err == nil {
		t.Fatal("Verify = nil, want a failure")
	}
	if !strings.Contains(logs.String(), "turnstile check failed") {
		t.Fatalf("failure not logged: %q", logs.String())
	}
}
