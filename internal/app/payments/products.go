// Product ids are catalog-to-env wiring, not adapter logic: one
// PUG_DODO_PRODUCT_<SLUG> per purchasable tier. They live here rather than in
// internal/deps/dodo because a deps package may see core only to implement one of
// its ports, and this implements none.
package payments

import (
	"fmt"
	"os"
	"strings"

	"github.com/pug-sh/pug/internal/core/billing/entitlement"
)

// One key per purchasable tier, so the key set follows the catalog rather than a
// list kept here.
const productEnvPrefix = "PUG_DODO_PRODUCT_"

// EnvLookup is os.LookupEnv, injected so ProductIDs is testable without setting
// process environment.
type EnvLookup func(string) (string, bool)

// ProductIDs resolves slug -> product id for every card that has one. A card with
// no key is simply not purchasable. The cards are a parameter rather than read
// from the catalog so the duplicate-product guard below stays exercisable while
// the real catalog holds only one card.
func ProductIDs(lookup EnvLookup, cards []entitlement.RateCard) (map[string]string, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	out := map[string]string{}
	byProduct := map[string]string{}
	// Every card, retired ones included: a retired card stays mapped or the webhook
	// rejects its holders' renewals. Free, trial and custom are states rather than
	// catalog entries, so excluding them is structural and needs no predicate.
	for _, card := range cards {
		key := productEnvPrefix + strings.ToUpper(strings.ReplaceAll(card.Slug, "-", "_"))
		id, ok := lookup(key)
		id = strings.TrimSpace(id)
		if !ok || id == "" {
			continue
		}
		// One product cannot back two cards: the webhook resolves a card by product id,
		// so a duplicate would silently pick one.
		if other, dup := byProduct[id]; dup {
			return nil, fmt.Errorf("dodo: %s and %s are configured with the same product id", other, card.Slug)
		}
		byProduct[id] = card.Slug
		out[card.Slug] = id
	}
	return out, nil
}
