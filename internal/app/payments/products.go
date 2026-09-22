package payments

import (
	"fmt"
	"strings"

	"github.com/pug-sh/pug/internal/core/billing/entitlement"
)

// One key per purchasable tier, so the key set follows the catalog rather than a
// list kept here.
const productEnvPrefix = "PUG_DODO_PRODUCT_"

// productIDs reads Dodo's product ids, one PUG_DODO_PRODUCT_<SLUG> per
// purchasable tier, into slug -> product id. A tier with no key is simply not
// purchasable. It is catalog-to-env wiring rather than adapter logic, so it lives
// here and not in internal/deps/dodo. lookup is os.LookupEnv outside tests.
func productIDs(lookup func(string) (string, bool)) (map[string]string, error) {
	out := map[string]string{}
	byProduct := map[string]string{}
	for _, plan := range entitlement.Plans() {
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
func mappedSlug(plan entitlement.Plan) bool {
	switch plan.Slug {
	case entitlement.SlugFree, entitlement.SlugTrial, entitlement.SlugCustom:
		return false
	}
	return true
}
