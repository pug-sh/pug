// Package payments builds the configured merchant of record. It is the one place
// PUG_BILLING_PROVIDER is dispatched, so the server and the reconcile pass cannot
// resolve the same variable differently, and it is where a second provider gets
// wired in beside the first.
//
// It sits in app/ rather than deps/ because it is wiring, not infrastructure: a
// deps package may see core only to implement one of its ports, and this
// implements none.
package payments

import (
	"context"
	"errors"
	"fmt"
	"strings"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
	"github.com/pug-sh/pug/internal/deps/dodo"
	"github.com/sethvargo/go-envconfig"
)

// ErrNoAPIKey is a provider named with no credentials behind it. Returned rather
// than decided here: the server degrades to "no buy button", while for the
// reconcile pass it is a misconfigured CronJob that must not exit 0.
var ErrNoAPIKey = errors.New("payments: provider named but no API key configured")

// New builds the named provider's wiring, or (nil, nil) when the name is empty --
// the self-hosted shape, where only the buy button is missing. ReturnURL is left
// unset: only a caller that starts checkouts knows where a buyer comes back to.
func New(ctx context.Context, providerName string) (*corebilling.Payments, error) {
	// Normalised once for every binary: PUG_BILLING_PROVIDER=DODO must not start one
	// and fail another.
	switch strings.ToLower(strings.TrimSpace(providerName)) {
	case "":
		return nil, nil
	case dodo.Name:
	default:
		return nil, fmt.Errorf("unknown PUG_BILLING_PROVIDER %q (want %q or empty)", providerName, dodo.Name)
	}

	var cfg dodo.Config
	if err := envconfig.Process(ctx, &cfg); err != nil {
		return nil, err
	}
	products, err := dodo.ProductIDs(nil)
	if err != nil {
		return nil, err
	}
	client, err := dodo.New(cfg)
	if err != nil {
		return nil, err
	}
	if client == nil {
		return nil, ErrNoAPIKey
	}

	// Derived from the one map so checkout's direction and the webhook's cannot
	// disagree about which tier a product is.
	slugByProduct := make(map[string]string, len(products))
	for slug, id := range products {
		slugByProduct[id] = slug
	}
	return &corebilling.Payments{
		ProductBySlug: products,
		Provider:      client,
		SlugByProduct: slugByProduct,
	}, nil
}
