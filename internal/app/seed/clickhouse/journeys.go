package seed

// ---------------------------------------------------------------------------
// Journeys
// ---------------------------------------------------------------------------

// step is one event in a journey template. arg carries step-specific context
// (page key for page_view, screen name for screen_view, checkout step name,
// form name, push campaign, ...).
type step struct {
	kind string
	arg  string
}

type journeyDef struct {
	name       string
	weight     int
	memberOnly bool // requires a signed-in member (push, billing, NPS)
	steps      []step
}

// journeyByName looks up a journey for episode-forced selections; panics on
// a typo, which the package tests catch immediately.
func journeyByName(defs []journeyDef, name string) journeyDef {
	for _, d := range defs {
		if d.name == name {
			return d
		}
	}
	panic("seed: unknown journey " + name)
}

// First-session journeys: not part of the random rotation — forced when a
// session lands within firstSessionWindow of the user's join, so signup and
// install events coincide with cohort entry.
var webSignupJourney = journeyDef{name: "signup", steps: []step{
	{"page_view", "home"}, {"product_viewed", ""}, {"page_view", "signup"},
	{"form_start", "signup"}, {"form_submit", "signup"}, {"signup", ""},
}}

var appInstallJourney = journeyDef{name: "app-install", steps: []step{
	{"app_install", ""}, {"app_open", ""}, {"signup", ""}, {"screen_view", "Home"},
	{"scroll", ""}, {"screen_view", "Product Detail"}, {"product_viewed", ""},
	{"app_close", ""},
}}

var webJourneys = []journeyDef{
	{name: "bounce", weight: 14, steps: []step{
		{"page_view", "home"}, {"scroll", ""},
	}},
	{name: "browse", weight: 22, steps: []step{
		{"page_view", "home"}, {"product_list_viewed", ""}, {"scroll", ""},
		{"product_viewed", ""}, {"scroll", ""}, {"product_viewed", ""},
	}},
	{name: "browse-filter", weight: 6, steps: []step{
		{"page_view", "home"}, {"product_list_viewed", ""}, {"filter_applied", ""},
		{"sort_changed", ""}, {"product_viewed", ""}, {"scroll", ""},
	}},
	{name: "search-browse", weight: 9, steps: []step{
		{"page_view", "home"}, {"search", ""}, {"search_result_clicked", ""},
		{"scroll", ""}, {"click", ""},
	}},
	{name: "recommendation", weight: 5, steps: []step{
		{"page_view", "home"}, {"recommendation_viewed", ""},
		{"recommendation_clicked", ""}, {"scroll", ""},
	}},
	{name: "purchase", weight: 2, steps: []step{
		{"page_view", "home"}, {"product_list_viewed", ""}, {"product_viewed", ""},
		{"add_to_cart", ""}, {"cart_viewed", ""}, {"checkout_started", ""},
		{"checkout_step_completed", "shipping"}, {"checkout_step_completed", "payment"},
		{"purchase", ""},
	}},
	{name: "purchase-multi-coupon", weight: 1, steps: []step{
		{"page_view", "home"}, {"search", ""}, {"search_result_clicked", ""},
		{"add_to_cart", ""}, {"product_viewed", ""}, {"add_to_cart", ""},
		{"cart_viewed", ""}, {"coupon_applied", ""}, {"checkout_started", ""},
		{"checkout_step_completed", "shipping"}, {"checkout_step_completed", "payment"},
		{"purchase", ""},
	}},
	{name: "abandoned-cart", weight: 7, steps: []step{
		{"page_view", "home"}, {"product_viewed", ""}, {"add_to_cart", ""},
		{"cart_viewed", ""},
	}},
	{name: "abandoned-checkout", weight: 4, steps: []step{
		{"page_view", "home"}, {"product_viewed", ""}, {"add_to_cart", ""},
		{"cart_viewed", ""}, {"checkout_started", ""},
		{"checkout_step_completed", "shipping"},
	}},
	{name: "wishlist", weight: 4, steps: []step{
		{"page_view", "home"}, {"product_list_viewed", ""}, {"product_viewed", ""},
		{"wishlist_added", ""}, {"product_viewed", ""},
	}},
	{name: "product-video", weight: 4, steps: []step{
		{"page_view", "home"}, {"product_viewed", ""}, {"video_started", ""},
		{"video_play", ""}, {"video_completed", ""},
	}},
	{name: "signin-browse", weight: 5, memberOnly: true, steps: []step{
		{"page_view", "home"}, {"page_view", "signin"}, {"signin", ""},
		{"page_view", "orders"}, {"product_list_viewed", ""}, {"product_viewed", ""},
	}},
	{name: "pupdates", weight: 2, steps: []step{
		{"page_view", "home"}, {"scroll", ""}, {"form_start", "pupdates"},
		{"form_submit", "pupdates"},
	}},
	{name: "help", weight: 2, steps: []step{
		{"page_view", "home"}, {"page_view", "help"}, {"help_article_viewed", ""},
	}},
	{name: "nps", weight: 1, memberOnly: true, steps: []step{
		{"page_view", "home"}, {"nps_submitted", ""},
	}},
	{name: "feedback", weight: 1, memberOnly: true, steps: []step{
		{"page_view", "orders"}, {"feedback_submitted", ""},
	}},
	{name: "frustration", weight: 2, steps: []step{
		{"page_view", "home"}, {"product_viewed", ""}, {"click", ""},
		{"dead_click", ""}, {"rage_click", ""},
	}},
	{name: "error", weight: 1, steps: []step{
		{"page_view", "home"}, {"product_viewed", ""}, {"error_occurred", ""},
	}},
	{name: "share", weight: 1, steps: []step{
		{"page_view", "home"}, {"product_viewed", ""}, {"share", ""},
	}},
	{name: "club-trial", weight: 1, memberOnly: true, steps: []step{
		{"page_view", "home"}, {"page_view", "club"}, {"scroll", ""},
		{"trial_started", ""},
	}},
	{name: "club-convert", weight: 1, memberOnly: true, steps: []step{
		{"page_view", "club"}, {"trial_converted", ""}, {"subscription_started", ""},
		{"invoice_paid", ""},
	}},
	{name: "refund", weight: 1, memberOnly: true, steps: []step{
		{"page_view", "orders"}, {"order_refunded", ""},
	}},
}

var appJourneys = []journeyDef{
	{name: "app-browse", weight: 22, steps: []step{
		{"app_open", ""}, {"screen_view", "Home"}, {"scroll", ""},
		{"screen_view", "Product Detail"}, {"product_viewed", ""}, {"scroll", ""},
		{"app_close", ""},
	}},
	{name: "app-search", weight: 8, steps: []step{
		{"app_open", ""}, {"screen_view", "Home"}, {"screen_view", "Search"},
		{"search", ""}, {"search_result_clicked", ""}, {"screen_view", "Product Detail"},
		{"app_close", ""},
	}},
	{name: "app-purchase", weight: 3, steps: []step{
		{"app_open", ""}, {"screen_view", "Home"}, {"screen_view", "Product Detail"},
		{"product_viewed", ""}, {"add_to_cart", ""}, {"screen_view", "Cart"},
		{"cart_viewed", ""}, {"checkout_started", ""},
		{"checkout_step_completed", "shipping"}, {"checkout_step_completed", "payment"},
		{"purchase", ""}, {"app_close", ""},
	}},
	{name: "app-abandoned-cart", weight: 6, steps: []step{
		{"app_open", ""}, {"screen_view", "Home"}, {"product_viewed", ""},
		{"add_to_cart", ""}, {"screen_view", "Cart"}, {"cart_viewed", ""},
		{"app_close", ""},
	}},
	{name: "push-recovery", weight: 2, memberOnly: true, steps: []step{
		{"notification_received", "abandoned-cart"}, {"notification_clicked", "abandoned-cart"},
		{"app_open", ""}, {"screen_view", "Cart"}, {"cart_viewed", ""},
		{"checkout_started", ""}, {"checkout_step_completed", "shipping"},
		{"checkout_step_completed", "payment"}, {"purchase", ""}, {"app_close", ""},
	}},
	{name: "push-browse", weight: 3, memberOnly: true, steps: []step{
		{"notification_received", ""}, {"notification_clicked", ""}, {"app_open", ""},
		{"screen_view", "Home"}, {"product_viewed", ""}, {"app_close", ""},
	}},
	{name: "push-dismissed", weight: 3, memberOnly: true, steps: []step{
		{"notification_received", ""}, {"notification_dismissed", ""},
	}},
	{name: "app-signin", weight: 4, memberOnly: true, steps: []step{
		{"app_open", ""}, {"screen_view", "Profile"}, {"signin", ""},
		{"screen_view", "Orders"}, {"app_close", ""},
	}},
	{name: "app-video", weight: 3, steps: []step{
		{"app_open", ""}, {"screen_view", "Product Detail"}, {"product_viewed", ""},
		{"video_started", ""}, {"video_play", ""}, {"video_pause", ""},
		{"app_close", ""},
	}},
	{name: "app-wishlist", weight: 3, steps: []step{
		{"app_open", ""}, {"screen_view", "Home"}, {"product_viewed", ""},
		{"wishlist_added", ""}, {"app_close", ""},
	}},
	{name: "app-background", weight: 3, steps: []step{
		{"app_open", ""}, {"screen_view", "Home"}, {"app_backgrounded", ""},
		{"app_foregrounded", ""}, {"screen_view", "Product Detail"}, {"app_close", ""},
	}},
	{name: "app-crash", weight: 1, steps: []step{
		{"app_open", ""}, {"screen_view", "Home"}, {"app_crashed", ""},
	}},
	{name: "app-nps", weight: 1, memberOnly: true, steps: []step{
		{"app_open", ""}, {"screen_view", "Home"}, {"nps_submitted", ""}, {"app_close", ""},
	}},
}
