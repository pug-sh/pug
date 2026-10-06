// Package turnstile checks Cloudflare Turnstile tokens with Siteverify.
package turnstile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/httpx"
	"github.com/pug-sh/pug/internal/slogx"
)

const (
	siteverifyURL = "https://challenges.cloudflare.com/turnstile/v0/siteverify"
	timeout       = 5 * time.Second
)

var ErrRejected = errors.New("turnstile token rejected")

var visitorCodes = []string{"invalid-input-response", "timeout-or-duplicate"}

type Verifier struct {
	siteKey string
	secret  string
	url     string
	client  *http.Client
}

func New(siteKey, secret string) *Verifier {
	return &Verifier{siteKey: siteKey, secret: secret, url: siteverifyURL, client: httpx.NewClient(timeout)}
}

func (v *Verifier) SiteKey() string { return v.siteKey }

// Verify returns ErrRejected for a missing or refused token; any other error means the check could not run.
func (v *Verifier) Verify(ctx context.Context, token string) error {
	if token == "" {
		return ErrRejected
	}
	err := v.siteverify(ctx, token)
	if err == nil || errors.Is(err, ErrRejected) {
		return err
	}
	// The caller went away, so Cloudflare is not at fault.
	if ctx.Err() != nil {
		return err
	}
	slog.ErrorContext(ctx, "turnstile check failed", slogx.Error(err))
	telemetry.RecordError(ctx, err)
	return err
}

func (v *Verifier) siteverify(ctx context.Context, token string) error {
	form := url.Values{"secret": {v.secret}, "response": {token}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.url, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := v.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var res struct {
		Success    bool     `json:"success"`
		ErrorCodes []string `json:"error-codes"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&res); err != nil {
		return fmt.Errorf("siteverify: status %d: decode response: %w", resp.StatusCode, err)
	}
	if resp.StatusCode == http.StatusOK && res.Success {
		return nil
	}
	// The status varies by error (a wrong secret is a 400, a bad token a 200), so the codes decide.
	if refusedToken(res.ErrorCodes) {
		return ErrRejected
	}
	return fmt.Errorf("siteverify: status %d, error codes %v", resp.StatusCode, res.ErrorCodes)
}

func refusedToken(codes []string) bool {
	for _, code := range codes {
		if !slices.Contains(visitorCodes, code) {
			return false
		}
	}
	return len(codes) > 0
}
