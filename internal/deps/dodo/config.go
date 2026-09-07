// Package dodo is the only package that imports the Dodo Payments SDK. Nothing
// above billing.PaymentProvider knows its payload shapes or signature scheme.
package dodo

import (
	"fmt"
	"os"
	"strings"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
)

// Name is the provider slug: stored on every row this package produces and the
// path segment the webhook mounts at, so changing it orphans rows and 404s.
const Name = "dodo"

const (
	EnvironmentTest = "test"
	EnvironmentLive = "live"
)

// productEnvPrefix maps a catalog slug to a Dodo product, one key per purchasable
// tier. Read straight from the environment: the key set is the catalog's.
const productEnvPrefix = "PUG_DODO_PRODUCT_"

// Config is the provider's own credentials, under its own prefix rather than a
// generic PUG_PAYMENTS_*, so a second provider's keys sit beside these.
type Config struct {
	APIKey        string `env:"PUG_DODO_API_KEY"`
	Environment   string `env:"PUG_DODO_ENVIRONMENT,default=test"`
	WebhookSecret string `env:"PUG_DODO_WEBHOOK_SECRET"`
}

// EnvLookup is os.LookupEnv, injected so the product map is testable without
// setting process environment.
type EnvLookup func(string) (string, bool)

// ProductIDs resolves slug -> product id for every catalog tier that has one. The
// floors are never sold and custom's product lives on the org's row. Both
// directions come from one map so checkout and the webhook cannot disagree.
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
		// One product cannot back two tiers: the webhook resolves a plan by product id,
		// so a duplicate would silently pick one.
		if other, dup := byProduct[id]; dup {
			return nil, fmt.Errorf("dodo: %s and %s are configured with the same product id", other, plan.Slug)
		}
		byProduct[id] = plan.Slug
		out[plan.Slug] = id
	}
	return out, nil
}

// mappedSlug is the catalog side of "can this tier have a product": every tier but
// the floors and custom. Retired tiers ARE mapped, or the webhook rejects their
// holders' renewals and cancellations; core is what keeps them unsellable.
func mappedSlug(plan corebilling.Plan) bool {
	switch plan.Slug {
	case corebilling.SlugFree, corebilling.SlugTrial, corebilling.SlugCustom:
		return false
	}
	return true
}
