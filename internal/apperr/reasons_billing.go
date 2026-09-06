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

// Confirmation reasons. All but the last describe a checkout that was PAID --
// the money is in -- so none may reach the buyer as a generic failure.
var (
	// A session id whose subscription names a different org, or none.
	ReasonBillingCheckoutNotForOrg = codes.add("BILLING_CHECKOUT_NOT_FOR_ORG")
	// Paid in a currency pug neither stores nor renders. A person has to act.
	ReasonBillingCurrencyUnsupported = codes.add("BILLING_CURRENCY_UNSUPPORTED")
	// Paid against a product no config key and no org row maps to. Distinct from
	// NOT_PURCHASABLE, which is a checkout refused before any money moved.
	ReasonBillingProductUnmapped = codes.add("BILLING_PRODUCT_UNMAPPED")
	// Paid while the org already holds a live subscription. The plan cannot be
	// applied without someone deciding which of the two survives.
	ReasonBillingTwoLiveSubscriptions = codes.add("BILLING_TWO_LIVE_SUBSCRIPTIONS")
	// The provider says the checkout will not settle -- a declined card, most
	// often. The one confirmation reason where no money moved.
	ReasonBillingCheckoutFailed = codes.add("BILLING_CHECKOUT_FAILED")
)
