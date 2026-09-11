package apperr

// Billing domain reasons.
var (
	ReasonBillingUnavailable    = codes.add("BILLING_UNAVAILABLE")
	ReasonBillingNotPurchasable = codes.add("BILLING_NOT_PURCHASABLE")
	ReasonBillingNoCustomer     = codes.add("BILLING_NO_CUSTOMER")
	ReasonBillingPlanNotFound   = codes.add("BILLING_PLAN_NOT_FOUND")

	// Confirmation reasons; confirmErr owns which of these answer a checkout whose
	// money is already in. PRODUCT_UNMAPPED and NOT_PURCHASABLE above translate the
	// same sentinel — one after money moved, one before.
	ReasonBillingCheckoutNotForOrg        = codes.add("BILLING_CHECKOUT_NOT_FOR_ORG")
	ReasonBillingCurrencyUnsupported      = codes.add("BILLING_CURRENCY_UNSUPPORTED")
	ReasonBillingProductUnmapped          = codes.add("BILLING_PRODUCT_UNMAPPED")
	ReasonBillingTwoLiveSubscriptions     = codes.add("BILLING_TWO_LIVE_SUBSCRIPTIONS")
	ReasonBillingCheckoutFailed           = codes.add("BILLING_CHECKOUT_FAILED")
	ReasonBillingSubscriptionUnapplicable = codes.add("BILLING_SUBSCRIPTION_UNAPPLICABLE")
	ReasonBillingNoMandate                = codes.add("BILLING_NO_MANDATE")
)
