package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/pug-sh/pug/internal/app/payments"
	corebilling "github.com/pug-sh/pug/internal/core/billing"
	"github.com/sethvargo/go-envconfig"
)

// paymentsConfig is what the server adds on top of the provider's own credentials,
// which live under that provider's prefix (see internal/deps/dodo).
type paymentsConfig struct {
	Provider string `env:"PUG_BILLING_PROVIDER"`
	// Reused rather than redeclared: this is the same "where the dashboard lives"
	// the email service sends links to, and a second variable would drift.
	DashboardBaseURL string `env:"PUG_DASHBOARD_BASE_URL"`
}

// checkoutReturnPath is where a buyer lands after checkout: the dashboard's own
// billing page, never a provider page.
const checkoutReturnPath = "/settings/billing"

// newPayments builds the provider wiring, or nil when none is configured — a
// supported mode where only the buy button is missing. An unrecognised provider
// name FAILS STARTUP: from the dashboard it is indistinguishable from disabled.
func newPayments(ctx context.Context) (*corebilling.Payments, error) {
	var cfg paymentsConfig
	if err := envconfig.Process(ctx, &cfg); err != nil {
		return nil, err
	}
	p, err := payments.New(ctx, cfg.Provider)
	if errors.Is(err, payments.ErrNoAPIKey) {
		slog.WarnContext(ctx, "billing provider named but no API key configured; checkout is unavailable",
			slog.String("provider", cfg.Provider))
		return nil, nil
	}
	if err != nil || p == nil {
		return nil, err
	}
	if !p.Provider.CanVerify() {
		// The route does not mount without a secret, so every delivery 404s and the
		// local state silently stops tracking the provider's.
		slog.WarnContext(ctx, "billing provider has no webhook secret; the webhook route will not mount",
			slog.String("provider", p.Provider.Name()))
	}
	if len(p.ProductBySlug) == 0 {
		slog.WarnContext(ctx, "billing provider configured with no product ids; no catalog tier is purchasable",
			slog.String("provider", p.Provider.Name()))
	}

	// Dodo rejects a relative return_url, so an unset PUG_DASHBOARD_BASE_URL would
	// fail every checkout at the provider instead of at startup.
	base := strings.TrimSuffix(cfg.DashboardBaseURL, "/")
	if u, err := url.Parse(base); err != nil || !u.IsAbs() || u.Host == "" {
		return nil, fmt.Errorf("PUG_BILLING_PROVIDER is set, so PUG_DASHBOARD_BASE_URL must be an absolute URL (got %q)", cfg.DashboardBaseURL)
	}
	p.ReturnURL = base + checkoutReturnPath
	return p, nil
}
