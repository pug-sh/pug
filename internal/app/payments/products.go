package payments

import (
	"fmt"
	"strings"

	"github.com/pug-sh/pug/internal/core/billing/entitlement"
)

// One key per catalog plan, so the key set follows the catalog rather than a list
// kept here.
const productEnvPrefix = "PUG_DODO_PRODUCT_"

// productIDs reads Dodo's product ids into slug -> product id: one
// PUG_DODO_PRODUCT_<SLUG> per plan, retired plans included — their holders still
// renew. A plan with no key is simply not purchasable. The plans are a parameter so
// the duplicate-product guard stays testable while the real catalog holds one plan.
// Free and custom are not catalog plans, so nothing needs excluding. It is
// catalog-to-env wiring rather than adapter logic, so it lives here and not in
// internal/deps/dodo. lookup is os.LookupEnv outside tests.
func productIDs(lookup func(string) (string, bool), plans []entitlement.Plan) (map[string]string, error) {
	out := map[string]string{}
	byProduct := map[string]string{}
	for _, plan := range plans {
		key := productEnvPrefix + strings.ToUpper(strings.ReplaceAll(plan.Slug, "-", "_"))
		id, ok := lookup(key)
		id = strings.TrimSpace(id)
		if !ok || id == "" {
			continue
		}
		// One product cannot back two plans: the webhook resolves a plan by product id,
		// so a duplicate would silently pick one.
		if other, dup := byProduct[id]; dup {
			return nil, fmt.Errorf("dodo: %s and %s are configured with the same product id", other, plan.Slug)
		}
		byProduct[id] = plan.Slug
		out[plan.Slug] = id
	}
	return out, nil
}
