package dodo

import (
	"fmt"
	"os"
	"strings"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
)

// One key per purchasable tier, so the key set follows the catalog rather than a
// list kept here.
const productEnvPrefix = "PUG_DODO_PRODUCT_"

// EnvLookup is os.LookupEnv, injected so ProductIDs is testable without setting
// process environment.
type EnvLookup func(string) (string, bool)

// ProductIDs resolves slug -> product id for every catalog tier that has one. A
// tier with no key is simply not purchasable.
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

// mappedSlug is every tier but the floors and custom. Retired tiers stay mapped,
// or the webhook rejects their holders' renewals; core keeps them unsellable.
func mappedSlug(plan corebilling.Plan) bool {
	switch plan.Slug {
	case corebilling.SlugFree, corebilling.SlugTrial, corebilling.SlugCustom:
		return false
	}
	return true
}
