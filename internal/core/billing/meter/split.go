// Package meter states each org's usage to the provider's meters. It splits a
// period's metered events across the plan's tiers, carries what a period ended
// short into the next, and writes ahead to billing_meter_periods — the only writer
// of that table — so the ledger never understates what the provider holds. It
// prices nothing: every rate lives on the provider's product.
package meter

// Split divides a period's event count into what falls in each tier, one entry
// per tier: tier k holds the events in [max(allowance, bound(k-1)), bound(k)),
// where bound(-1) is 0 and the last tier is unbounded. Events below the allowance
// fall in no tier, so they are never reported and never billed.
//
// Tier 1 therefore starts where the org's own allowance ends, not at the plan's
// default: a deal allowing fewer events than the default still bills from where
// its allowance ends. The tiers always sum to max(0, total-allowance).
func Split(total, allowance int64, tierUpTo []int64) []int64 {
	out := make([]int64, len(tierUpTo)+1)
	var bound int64
	for k := range out {
		lo := max(bound, allowance)
		hi := total
		if k < len(tierUpTo) {
			hi = min(total, tierUpTo[k])
			bound = tierUpTo[k]
		}
		out[k] = max(0, hi-lo)
	}
	return out
}
