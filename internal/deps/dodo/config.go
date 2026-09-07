// Package dodo is the only package that imports the Dodo Payments SDK. Nothing
// above billing.PaymentProvider knows the provider's name, its payload shapes or
// its signature scheme.
package dodo

import (
	"fmt"
	"os"
	"strings"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
)

// Name is the provider slug. It is stored on every row this package produces and
// it is the path segment the webhook mounts at, so it is an identifier: changing
// it orphans stored rows and silently 404s the provider's deliveries.
const Name = "dodo"

const (
	EnvironmentTest = "test"
	EnvironmentLive = "live"
)

// productEnvPrefix maps a catalog slug to a Dodo product. One key per purchasable
// tier; a tier with no key is simply not purchasable. Read from the environment
// directly rather than through envconfig because the key set is the catalog's,
// so a new tier needs a deploy variable and no code change here.
const productEnvPrefix = "PUG_DODO_PRODUCT_"

// Config is the provider's own credentials, under its own prefix rather than a
// generic PUG_PAYMENTS_*: a second provider's keys then sit beside these instead
// of overwriting them, which is what lets both be configured during a cutover.
type Config struct {
	APIKey        string `env:"PUG_DODO_API_KEY"`
	Environment   string `env:"PUG_DODO_ENVIRONMENT,default=test"`
	WebhookSecret string `env:"PUG_DODO_WEBHOOK_SECRET"`
}

// EnvLookup is os.LookupEnv, injected so the product map is testable without
// setting process environment.
type EnvLookup func(string) (string, bool)

// ProductIDs resolves slug -> product id for every catalog tier that has one.
// The floors are never sold and `custom` gets its product from the org's own row,
// so neither is looked up. A retired tier keeps its key: its holders' renewals
// and cancellations still have to be placeable.
//
// Both directions matter: checkout reads slug -> product, and the webhook reads
// product -> slug. They come from one map so they cannot disagree.
func ProductIDs(lookup EnvLookup) (map[string]string, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	out := map[string]string{}
	byProduct := map[string]string{}
	for _, plan := range corebilling.Plans() {
		if !mappedSlug(plan) {
			continue
		}
		key := productEnvPrefix + strings.ToUpper(strings.ReplaceAll(plan.Slug, "-", "_"))
		id, ok := lookup(key)
		id = strings.TrimSpace(id)
		if !ok || id == "" {
			continue
		}
		// One product cannot back two tiers: the webhook resolves a plan by product
		// id, so a duplicate would make an incoming subscription's tier ambiguous and
		// pick one silently.
		if other, dup := byProduct[id]; dup {
			return nil, fmt.Errorf("dodo: %s and %s are configured with the same product id", other, plan.Slug)
		}
		byProduct[id] = plan.Slug
		out[plan.Slug] = id
	}
	return out, nil
}

// mappedSlug is the catalog side of "can this tier have a product": every tier
// except the floors and custom, whose product lives on the org's row.
//
// Retired tiers ARE mapped, deliberately. The map's other direction is how the
// webhook places a delivery, so dropping a retired tier would reject its existing
// holders' renewals AND cancellations as unmappable -- permanently, since that
// rejection marks the delivery processed. Nothing becomes sellable: core filters
// Retired in CreateCheckoutSession and PlanOptions.
func mappedSlug(plan corebilling.Plan) bool {
	switch plan.Slug {
	case corebilling.SlugFree, corebilling.SlugTrial, corebilling.SlugCustom:
		return false
	}
	return true
}
