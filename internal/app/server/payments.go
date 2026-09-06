package server

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
	"github.com/pug-sh/pug/internal/deps/dodo"
	"github.com/sethvargo/go-envconfig"
)

// paymentsConfig picks the merchant of record to construct. Credentials stay
// under each provider's own prefix, so a second provider's keys sit beside the
// first's rather than overwriting them -- which is what lets both be configured
// at once during a cutover. Only this decides which is constructed for checkout;
// a webhook route mounts for every provider whose secret is present.
type paymentsConfig struct {
	Provider string `env:"PUG_BILLING_PROVIDER"`
	// Reused rather than redeclared: this is the same "where the dashboard lives"
	// the email service sends links to, and a second variable would drift.
	DashboardBaseURL string `env:"PUG_DASHBOARD_BASE_URL"`
}

// checkoutReturnPath is where a buyer lands after checkout: the dashboard's own
// billing page, never a provider page.
const checkoutReturnPath = "/settings/billing"

// newPayments builds the provider wiring, or nil when none is configured.
// Billing enabled with no payments credentials is a supported mode: quotas,
// grants and comped deals all work and only the buy button is missing.
//
// An unrecognised provider name FAILS STARTUP rather than silently disabling
// checkout, because from the dashboard the two are indistinguishable.
func newPayments(ctx context.Context) (*corebilling.Payments, bool, error) {
	var cfg paymentsConfig
	if err := envconfig.Process(ctx, &cfg); err != nil {
		return nil, false, err
	}
	name := strings.ToLower(strings.TrimSpace(cfg.Provider))
	if name == "" {
		return nil, false, nil
	}
	if name != dodo.Name {
		return nil, false, fmt.Errorf("unknown PUG_BILLING_PROVIDER %q (want %q or empty)", cfg.Provider, dodo.Name)
	}

	var dodoCfg dodo.Config
	if err := envconfig.Process(ctx, &dodoCfg); err != nil {
		return nil, false, err
	}
	products, err := dodo.ProductIDs(nil)
	if err != nil {
		return nil, false, err
	}
	client, err := dodo.New(dodoCfg, products)
	if err != nil {
		return nil, false, err
	}
	if client == nil {
		slog.WarnContext(ctx, "billing provider named but no API key configured; checkout is unavailable",
			slog.String("provider", name))
		return nil, false, nil
	}
	if !client.CanVerify() {
		// The route does not mount without a secret, so every delivery 404s and the
		// local state silently stops tracking the provider's.
		slog.WarnContext(ctx, "billing provider has no webhook secret; the webhook route will not mount",
			slog.String("provider", name))
	}
	if len(products) == 0 {
		slog.WarnContext(ctx, "billing provider configured with no product ids; no catalog tier is purchasable",
			slog.String("provider", name))
	}

	slugByProduct := make(map[string]string, len(products))
	for slug, id := range products {
		slugByProduct[id] = slug
	}
	return &corebilling.Payments{
		ProductBySlug: products,
		Provider:      client,
		ReturnURL:     strings.TrimSuffix(cfg.DashboardBaseURL, "/") + checkoutReturnPath,
		SlugByProduct: slugByProduct,
	}, client.CanVerify(), nil
}
