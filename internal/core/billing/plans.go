package billing

// Plan is a catalog tier. The catalog is Go rather than rows: a tier is static
// product config with revenue consequences, so it belongs in review and deploy
// — and a code catalog means no seed step and no catalog row that signup could
// depend on. See docs/architecture/billing.md section 4.
type Plan struct {
	Slug        string
	DisplayName string
	// ISO 4217. Mandatory: PriceCents is minor units of THIS currency, and minor
	// units are not always hundredths (JPY has none, KWD has three), so nothing
	// may format an amount without it.
	Currency string
	// nil means there is no price to show — the custom tier, whose price lives on
	// the org's own row. Distinct from 0, which is a real price (a comped deal).
	PriceCents *int64
	// nil means the tier carries no quota of its own, again only the custom tier.
	// Never a sentinel: a number that reads as a quota invites arithmetic that
	// produces "0 events remaining".
	IncludedEvents *int64
	// Retired tiers stay in the catalog so existing holders keep resolving, but are
	// never granted to a new org — without this, repricing (which mints a new slug
	// and retires the old one) would go on handing out the superseded numbers.
	Retired bool
}

// Slugs the resolution rules name directly. The rest of the catalog is data to
// everything in this package.
const (
	SlugFree   = "free"
	SlugTrial  = "trial"
	SlugCustom = "custom"
)

// TrialDays is how long a new org trials for, measured from orgs.create_time.
// The trial is the org's age, not stored state — nothing is written at signup.
const TrialDays = 14

// MaxTrialDays caps one extend-trial. A trial is a sales tool measured in weeks;
// past this the operator wants a comped plan, which has a price and a record.
const MaxTrialDays = 365

// catalog is every tier pug has ever sold, newest last.
//
// IMMUTABILITY: a tier's Currency, PriceCents and IncludedEvents are fixed once
// any org holds it. Editing them re-negotiates every live agreement on that tier
// with a one-line diff and no record; repricing mints a new slug (growth-v2) and
// marks the old one Retired. DisplayName is the exception — renaming "Growth" to
// "Team" changes nothing anybody bought. TestCatalogIsPinned fails on an edit to
// the fixed fields, which is the only guard that exists against a silent quota
// cut. See docs/architecture/billing.md section 4.2.
var catalog = []Plan{
	{Slug: SlugFree, DisplayName: "Free", Currency: "USD", PriceCents: i64(0), IncludedEvents: i64(10_000)},
	{Slug: SlugTrial, DisplayName: "Trial", Currency: "USD", PriceCents: i64(0), IncludedEvents: i64(500_000)},
	{Slug: "starter", DisplayName: "Starter", Currency: "USD", PriceCents: i64(1_000), IncludedEvents: i64(100_000)},
	{Slug: "growth", DisplayName: "Growth", Currency: "USD", PriceCents: i64(2_000), IncludedEvents: i64(500_000)},
	{Slug: "scale", DisplayName: "Scale", Currency: "USD", PriceCents: i64(3_000), IncludedEvents: i64(1_000_000)},
	// No price and no quota of its own: a negotiated deal supplies both from the
	// org's row, which the billing_entitlements_custom_needs_quota constraint
	// makes mandatory.
	{Slug: SlugCustom, DisplayName: "Custom", Currency: "USD"},
}

// mustPlan is for the floors only. A missing floor resolves EVERY org to a blank
// plan with no quota at all, silently and with a 200, so it is not a miss any
// caller can sensibly handle.
func mustPlan(slug string) Plan {
	p, ok := PlanBySlug(slug)
	if !ok {
		panic("billing: catalog is missing the floor plan " + slug)
	}
	return p
}

// Plans returns the catalog in display order.
func Plans() []Plan {
	out := make([]Plan, 0, len(catalog))
	for _, p := range catalog {
		out = append(out, copyPlan(p))
	}
	return out
}

// PlanBySlug resolves a stored slug. Reports false for a slug the catalog no
// longer knows, which the caller must treat as "no quota" rather than as free —
// see Resolve.
func PlanBySlug(slug string) (Plan, bool) {
	for _, p := range catalog {
		if p.Slug == slug {
			return copyPlan(p), true
		}
	}
	return Plan{}, false
}

// copyPlan detaches the pointer fields, without which a caller writing through
// a returned *int64 would reprice the catalog for the whole process.
func copyPlan(p Plan) Plan {
	if p.PriceCents != nil {
		p.PriceCents = i64(*p.PriceCents)
	}
	if p.IncludedEvents != nil {
		p.IncludedEvents = i64(*p.IncludedEvents)
	}
	return p
}

// isFloor reports the two tiers nothing is ever charged for. Everything else is
// a plan an org holds because somebody agreed to it.
func (p Plan) isFloor() bool {
	return p.Slug == SlugFree || p.Slug == SlugTrial
}

func i64(v int64) *int64 { return &v }
