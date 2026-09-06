package apperr

// Billing domain reasons.
var (
	// The deployment has no payments provider at all -- the self-hosted shape,
	// where quotas and grants still work and only checkout is missing.
	ReasonBillingUnavailable = codes.add("BILLING_UNAVAILABLE")
	// Nothing to check out against: an unconfigured catalog tier, or a negotiated
	// deal whose product id nobody has recorded on the org yet.
	ReasonBillingNotPurchasable = codes.add("BILLING_NOT_PURCHASABLE")
	// A portal asked for by an org that has never checked out, so it has no
	// customer at the provider.
	ReasonBillingNoCustomer   = codes.add("BILLING_NO_CUSTOMER")
	ReasonBillingPlanNotFound = codes.add("BILLING_PLAN_NOT_FOUND")
)
