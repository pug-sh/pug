package billing

// Plan is a catalog tier. The catalog is Go rather than rows so a change to what
// a tier costs or includes goes through review and deploy.
type Plan struct {
	Slug        string
	DisplayName string
	// ISO 4217. PriceCents is minor units of THIS currency, and minor units are not
	// always hundredths (JPY has none, KWD has three).
	Currency string
	// nil means there is no price to show — the custom tier, whose price lives in
	// the payments provider. Distinct from 0, which is a real price (the floors).
	PriceCents *int64
	// nil means the tier carries no quota of its own, again only the custom tier.
	// Never a sentinel: a number that reads as a quota invites arithmetic that
	// produces "0 events remaining".
	IncludedEvents *int64
	// How far back the tier's history stays queryable; nil means no bound at all.
	RetentionDays *int64
	// A retired tier still resolves for existing holders but is never granted to a
	// new org, so repricing cannot go on handing out the superseded numbers.
	Retired bool
}

const (
	SlugFree   = "free"
	SlugTrial  = "trial"
	SlugCustom = "custom"
)

// RetentionYearDays is a flat 365 days, so a leap year cannot shorten a term
// somebody bought.
const RetentionYearDays = 365

// TrialDays is measured from orgs.create_time — the trial is the org's age, not
// stored state, so nothing is written at signup.
const TrialDays = 14

// MaxTrialDays caps one extend-trial. Past this the operator wants a comped plan,
// which has a price and a record.
const MaxTrialDays = 365

// catalog is every tier pug has ever sold, newest last.
//
// A tier's Currency, PriceCents, IncludedEvents and RetentionDays are fixed once
// any org holds it; repricing mints a new slug (growth-v2) and retires the old.
// TestCatalogIsPinned carries the reasoning and is the guard.
var catalog = []Plan{
	{Slug: SlugFree, DisplayName: "Free", Currency: "USD", PriceCents: i64(0),
		IncludedEvents: i64(10_000), RetentionDays: i64(RetentionYearDays)},
	{Slug: SlugTrial, DisplayName: "Trial", Currency: "USD", PriceCents: i64(0),
		IncludedEvents: i64(500_000), RetentionDays: i64(RetentionYearDays)},
	{Slug: "starter", DisplayName: "Starter", Currency: "USD", PriceCents: i64(1_000),
		IncludedEvents: i64(100_000), RetentionDays: i64(RetentionYearDays)},
	{Slug: "growth", DisplayName: "Growth", Currency: "USD", PriceCents: i64(2_000),
		IncludedEvents: i64(500_000), RetentionDays: i64(3 * RetentionYearDays)},
	{Slug: "scale", DisplayName: "Scale", Currency: "USD", PriceCents: i64(3_000),
		IncludedEvents: i64(1_000_000), RetentionDays: i64(7 * RetentionYearDays)},
	// A negotiated deal supplies all three from the org's row, where a constraint
	// makes the quota mandatory.
	{Slug: SlugCustom, DisplayName: "Custom", Currency: "USD"},
}

// mustPlan is for the floors only, inside the pure Resolve path, which has no
// error to return. NewService checks they exist at wiring time.
func mustPlan(slug string) Plan {
	p, ok := PlanBySlug(slug)
	if !ok {
		panic("billing: catalog is missing the floor plan " + slug)
	}
	return p
}

// Plans returns the catalog in display order, retired tiers included. A grant
// path must filter on Retired.
func Plans() []Plan {
	out := make([]Plan, 0, len(catalog))
	for _, p := range catalog {
		out = append(out, copyPlan(p))
	}
	return out
}

// PlanBySlug reports false for a slug the catalog no longer knows, which the
// caller must treat as "no quota" rather than as free — see Resolve.
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
	if p.RetentionDays != nil {
		p.RetentionDays = i64(*p.RetentionDays)
	}
	return p
}

func (p Plan) isFloor() bool {
	return p.Slug == SlugFree || p.Slug == SlugTrial
}

func i64(v int64) *int64 { return &v }
