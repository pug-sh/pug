package seed

import (
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Session construction
// ---------------------------------------------------------------------------

type cartLine struct {
	p   product
	qty int
}

// sessionState carries cart/product/checkout context across a session's
// steps so funnel events reference consistent ids and amounts.
type sessionState struct {
	user  *userProfile
	prof  deviceProfile
	start time.Time
	mem   *userMemory // cross-session memory; may be nil (bots, tests)

	page      string
	pageTitle string

	product    *product
	cart       []cartLine
	cartID     string
	checkoutID string
	discount   float64
	query      string
	planIdx    int  // 0 = unset; clubPlans index+1 so a billing funnel stays on one plan
	purchased  bool // whether this session completed a purchase
}

func buildSession(u *userProfile, prof deviceProfile, jd journeyDef, sessionStart, end time.Time, mem *userMemory) []event {
	if len(jd.steps) == 0 {
		return nil
	}

	st := &sessionState{
		user: u, prof: prof, start: sessionStart, mem: mem,
		page: "/", pageTitle: "Pug & Pals — By dogs, for dogs",
	}
	stableProps := buildSessionProps(u, prof, sessionStart)
	sessionID := uuid.New().String()

	occur := sessionStart
	sess := make([]event, 0, len(jd.steps)+2)
	// emit appends one event at the current occur time, stamping the live
	// web page (url/title) and attaching the referrer to the very first event.
	emit := func(kind string, customProps map[string]any) {
		autoProps := copyProps(stableProps)
		if prof.platform == "web" {
			autoProps["$url"] = storeURL + st.page
			autoProps["$pageTitle"] = st.pageTitle
			if len(sess) == 0 {
				if r, ok := autoProps["$referrerCandidate"]; ok && r != "" {
					autoProps["$referrer"] = r
				}
			}
		}
		delete(autoProps, "$referrerCandidate")
		applyAttribution(autoProps)

		sess = append(sess, event{
			eventID:          uuid.New().String(),
			distinctID:       u.id,
			sessionID:        sessionID,
			kind:             kind,
			occurTime:        clampOccurTime(occur, end),
			autoProperties:   autoProps,
			customProperties: customProps,
		})
	}

	for i, stp := range jd.steps {
		if i > 0 {
			occur = occur.Add(time.Duration(8+rand.Int64N(95)) * time.Second)
		}
		prevPage := st.page
		customProps := st.apply(stp)

		// A real storefront fires a page_view on every page load; the semantic
		// event (product_viewed, cart_viewed, …) follows on the same page. Emit
		// that page_view for web navigations that landed on a new page so
		// page_view stays the dominant event kind instead of scroll. The
		// explicit {page_view} steps already are page_views, so skip those.
		if prof.platform == "web" && stp.kind != "page_view" && st.page != prevPage {
			emit("page_view", map[string]any{})
			occur = occur.Add(time.Duration(1+rand.Int64N(4)) * time.Second)
		}
		emit(stp.kind, customProps)
	}

	// A cart left un-purchased is remembered so a later session (push
	// recovery or an organic return to the cart) can pick it up.
	if st.mem != nil && len(st.cart) > 0 && !st.purchased {
		st.mem.rememberAbandonedCart(st.cart, sessionStart)
	}
	// Record that the user has signed up so journeyFor won't force the signup
	// journey again. Keyed on the journey name (set even if the session was
	// truncated mid-build) so the cap holds regardless of step count.
	if st.mem != nil && (jd.name == webSignupJourney.name || jd.name == appInstallJourney.name) {
		st.mem.signedUp = true
	}
	return sess
}

// buildSessionProps returns auto-props that stay constant within a session.
func buildSessionProps(u *userProfile, prof deviceProfile, sessionStart time.Time) map[string]any {
	props := map[string]any{
		"$platform":   prof.platform,
		"$os":         prof.os,
		"$locale":     u.geo.locale,
		"$sdkVersion": sdkVersions[prof.platform],
		"$bot_score":  70 + rand.IntN(30), // humans score high
	}
	if v := prof.osVersions[rand.IntN(len(prof.osVersions))]; v != "" {
		props["$osVersion"] = v
	}
	if prof.browser != "" {
		props["$browser"] = prof.browser
		props["$browserVersion"] = prof.browserVersions[rand.IntN(len(prof.browserVersions))]
	}
	if prof.device != "" {
		props["$device"] = prof.device
	}
	props["$mobile"] = prof.mobile

	screen := prof.screens[rand.IntN(len(prof.screens))]
	props["$screenWidth"] = screen[0]
	props["$screenHeight"] = screen[1]

	if prof.platform != "web" {
		props["$app_version"] = appVersionAt(sessionStart)
	}

	if prof.platform == "web" {
		// ~25% of web sessions arrive via a campaign; promos push that share
		// up and stamp their own campaign name on most campaign traffic.
		promoName, promo := activePromo(sessionStart)
		acquisition := float64(0.25)
		if promo {
			acquisition = promoAcquisitionShare
		}
		if rand.Float64() < acquisition {
			utm := utmEntries[rand.IntN(len(utmEntries))]
			props["$utmSource"] = utm.source
			props["$utmMedium"] = utm.medium
			if promo && rand.Float64() < promoCampaignShare {
				props["$utmCampaign"] = promoName
			} else {
				props["$utmCampaign"] = utmCampaigns[rand.IntN(len(utmCampaigns))]
			}
			if utm.term != "" {
				props["$utmTerm"] = utm.term
			}
			if utm.content != "" {
				props["$utmContent"] = utm.content
			}
			props["$referrerCandidate"] = utm.referrer
		} else {
			props["$referrerCandidate"] = organicReferrers[rand.IntN(len(organicReferrers))]
		}
	}

	props["$continent"] = u.geo.continent
	props["$country"] = u.geo.country
	props["$region"] = u.geo.region
	props["$city"] = u.geo.city
	props["$postalCode"] = u.geo.postalCode
	props["$timezone"] = u.geo.timezone
	// Jitter coordinates ~±2km so live-map dots scatter around the city
	// instead of stacking on one point.
	props["$latitude"] = round5(u.geo.latitude + (rand.Float64()-0.5)*0.04)
	props["$longitude"] = round5(u.geo.longitude + (rand.Float64()-0.5)*0.04)

	return props
}

var webPages = map[string][2]string{
	"home":   {"/", "Pug & Pals — By dogs, for dogs"},
	"cart":   {"/cart", "Your Cart"},
	"search": {"/search", "Search"},
	"signup": {"/signup", "Create Account"},
	"signin": {"/signin", "Sign In"},
	"orders": {"/account/orders", "Order History"},
	"club":   {"/club", "Pug Club"},
	"help":   {"/help", "Help Center"},
}

var screenClasses = map[string]string{
	"ios":     "ViewController",
	"android": "Activity",
}

// apply executes one journey step: mutates session state (current page,
// cart, checkout ids) and returns the event's custom properties shaped per
// the well-known event catalog.
func (st *sessionState) apply(stp step) map[string]any {
	switch stp.kind {
	case "page_view":
		if p, ok := webPages[stp.arg]; ok {
			st.page, st.pageTitle = p[0], p[1]
		}
		return map[string]any{}

	case "screen_view":
		name := stp.arg
		class := strings.ReplaceAll(name, " ", "") + screenClasses[st.prof.platform]
		return map[string]any{"screen_name": name, "screen_class": class}

	case "product_list_viewed":
		cat := categories[rand.IntN(len(categories))]
		st.page = "/collections/" + cat
		st.pageTitle = strings.ToUpper(cat[:1]) + cat[1:]
		return map[string]any{
			"list_id":    "collection-" + cat,
			"list_name":  st.pageTitle,
			"category":   cat,
			"item_count": len(byCategory[cat]),
		}

	case "product_viewed":
		p := st.pickProduct()
		return map[string]any{
			"product_id": p.id, "product_name": p.name, "category": p.category,
			"brand": p.brand, "sku": p.sku, "price": p.price, "currency": "USD",
		}

	case "add_to_cart":
		p := st.currentProduct()
		// Long-tail basket sizes: mostly singles, occasionally a stock-up.
		var qty int
		switch r := rand.Float32(); {
		case r < 0.70:
			qty = 1
		case r < 0.90:
			qty = 2
		case r < 0.97:
			qty = 3
		default:
			qty = 4 + rand.IntN(3)
		}
		st.cart = append(st.cart, cartLine{p: *p, qty: qty})
		return map[string]any{
			"product_id": p.id, "price": p.price, "currency": "USD",
			"cart_id": st.ensureCartID(), "quantity": qty,
			"category": p.category, "brand": p.brand, "sku": p.sku,
		}

	case "remove_from_cart":
		if len(st.cart) == 0 {
			st.ensureCart()
		}
		line := st.cart[len(st.cart)-1]
		st.cart = st.cart[:len(st.cart)-1]
		return map[string]any{
			"product_id": line.p.id, "price": line.p.price, "currency": "USD",
			"cart_id": st.ensureCartID(), "quantity": line.qty,
			"category": line.p.category, "brand": line.p.brand, "sku": line.p.sku,
		}

	case "cart_viewed":
		st.ensureCart()
		if p, ok := webPages["cart"]; ok && st.prof.platform == "web" {
			st.page, st.pageTitle = p[0], p[1]
		}
		return map[string]any{
			"cart_id": st.ensureCartID(), "item_count": st.cartCount(),
			"amount": st.cartTotal(), "currency": "USD",
		}

	case "wishlist_added":
		p := st.currentProduct()
		return map[string]any{
			"product_id": p.id, "wishlist_id": "wl-" + st.user.id,
			"price": p.price, "currency": "USD",
		}

	case "coupon_applied":
		c := coupons[rand.IntN(len(coupons))]
		st.discount = c.discount
		return map[string]any{
			"coupon_id": c.id, "coupon_code": c.code, "cart_id": st.ensureCartID(),
			"discount_amount": c.discount, "currency": "USD",
		}

	case "checkout_started":
		st.ensureCart()
		st.checkoutID = "chk-" + shortID()
		if st.prof.platform == "web" {
			st.page, st.pageTitle = "/checkout", "Checkout"
		}
		return map[string]any{
			"amount": st.cartTotal(), "currency": "USD",
			"cart_id": st.ensureCartID(), "checkout_id": st.checkoutID,
			"item_count": st.cartCount(),
		}

	case "checkout_step_completed":
		idx := map[string]int{"shipping": 0, "payment": 1, "review": 2}[stp.arg]
		if st.checkoutID == "" {
			st.checkoutID = "chk-" + shortID()
		}
		return map[string]any{"checkout_id": st.checkoutID, "step": stp.arg, "step_index": idx}

	case "purchase":
		st.ensureCart()
		st.purchased = true
		first := st.cart[0].p
		amount := math.Max(round2(st.cartTotal()-st.discount), 0.99)
		orderID := "ord-" + shortID()
		if st.mem != nil {
			st.mem.rememberOrder(pastOrder{id: orderID, amount: amount, t: st.start})
		}
		return map[string]any{
			"product_id": first.id, "amount": amount, "currency": "USD",
			"order_id": orderID, "quantity": st.cartCount(),
			"category": first.category, "brand": first.brand, "sku": first.sku,
		}

	case "order_refunded":
		reasons := []string{"damaged", "wrong_size", "changed_mind", "late_delivery"}
		// Refund a real past order when the user has one; otherwise fall back
		// to a synthetic order so refund volume survives out-of-order batch
		// generation.
		if st.mem != nil {
			if o, ok := st.mem.takeOrderBefore(st.start); ok {
				return map[string]any{
					"order_id": o.id, "amount": o.amount, "currency": "USD",
					"reason": reasons[rand.IntN(len(reasons))],
				}
			}
		}
		p := catalog[weightedIndex(catalogWeights)]
		return map[string]any{
			"order_id": "ord-" + shortID(), "amount": p.price, "currency": "USD",
			"reason": reasons[rand.IntN(len(reasons))],
		}

	case "search":
		st.query = searchQueries[rand.IntN(len(searchQueries))]
		if st.prof.platform == "web" {
			st.page, st.pageTitle = "/search?q="+strings.ReplaceAll(st.query, " ", "+"), "Search"
		}
		return map[string]any{"query": st.query}

	case "search_result_clicked":
		if st.query == "" {
			st.query = searchQueries[rand.IntN(len(searchQueries))]
		}
		p := st.pickProduct()
		return map[string]any{"query": st.query, "result_id": p.id, "index": rand.IntN(8)}

	case "recommendation_viewed":
		p := catalog[weightedIndex(catalogWeights)]
		return map[string]any{
			"recommendation_id": "rec-homepage", "item_id": p.id,
			"source": "homepage", "index": rand.IntN(6),
		}

	case "recommendation_clicked":
		p := st.pickProduct()
		return map[string]any{
			"recommendation_id": "rec-homepage", "item_id": p.id,
			"source": "homepage", "index": rand.IntN(6),
		}

	case "filter_applied":
		filters := [][2]string{{"size", "S"}, {"size", "L"}, {"flavor", "chicken"}, {"flavor", "salmon"}, {"breed-size", "small-breed"}, {"price", "under-25"}, {"rating", "4-plus"}}
		fl := filters[rand.IntN(len(filters))]
		return map[string]any{"key": fl[0], "value": fl[1]}

	case "sort_changed":
		dirs := []string{"asc", "desc"}
		return map[string]any{"key": "price", "direction": dirs[rand.IntN(len(dirs))]}

	case "form_start":
		return map[string]any{"form_id": stp.arg + "-form", "form_name": stp.arg}

	case "form_submit":
		return map[string]any{"form_id": stp.arg + "-form", "form_name": stp.arg, "action": "/api/" + stp.arg}

	case "video_started", "video_completed":
		return map[string]any{"video_id": st.videoID()}

	case "video_play", "video_pause":
		return map[string]any{"video_id": st.videoID(), "position": fmt.Sprintf("%ds", rand.IntN(90))}

	case "notification_received", "notification_clicked", "notification_dismissed":
		campaign := stp.arg
		if campaign == "" {
			campaign = pushCampaigns[rand.IntN(len(pushCampaigns))]
		}
		return map[string]any{"campaign_id": "camp-" + campaign, "notification_type": "push"}

	case "nps_submitted":
		// Skew toward promoters with a detractor tail.
		scores := []int{10, 10, 9, 9, 9, 8, 8, 7, 6, 5, 3, 2}
		return map[string]any{"score": scores[rand.IntN(len(scores))]}

	case "feedback_submitted":
		reviews := []string{
			"Five stars. Ate the box too.",
			"The squeaky duck went silent after surgery. 10/10 would operate again.",
			"Ordered one tennis ball. Where are the other 63?",
			"Harness makes me look professional. Squirrels now respect me.",
			"Delivery human was very pettable. Subscribing.",
			"The forbidden sock is everything I dreamed of.",
			"Slow feeder bowl is a scam. I am simply faster now.",
			"Bed claims to be my size. It is. I still sleep on the human's side.",
			"Treats arrived. Memory of treats also arrived. Send more.",
			"GPS tag works great, my human found me at the cat's house in minutes.",
		}
		return map[string]any{
			"feedback_id": "fb-" + shortID(),
			"category":    "product",
			"comment":     reviews[rand.IntN(len(reviews))],
		}

	case "help_article_viewed":
		a := helpArticles[rand.IntN(len(helpArticles))]
		if st.prof.platform == "web" {
			st.page, st.pageTitle = "/help/"+a.id, a.title
		}
		return map[string]any{"article_id": a.id, "article_title": a.title, "category": a.category}

	case "trial_started":
		p := st.plan()
		trialID := "trial-" + shortID()
		if st.mem != nil {
			st.mem.rememberTrial(trialID, st.start)
		}
		return map[string]any{"trial_id": trialID, "plan_id": p.id, "plan_name": p.name}

	case "trial_converted":
		p := st.plan()
		trialID := "trial-" + shortID()
		if st.mem != nil {
			if id, ok := st.mem.takeTrialBefore(st.start); ok {
				trialID = id // convert the trial that was actually started
			}
		}
		return map[string]any{"trial_id": trialID, "subscription_id": "sub-" + st.user.id, "plan_id": p.id, "plan_name": p.name}

	case "subscription_started":
		p := st.plan()
		return map[string]any{"subscription_id": "sub-" + st.user.id, "plan_id": p.id, "plan_name": p.name, "amount": p.amount, "currency": "USD"}

	case "invoice_paid":
		p := st.plan()
		return map[string]any{"invoice_id": "inv-" + shortID(), "subscription_id": "sub-" + st.user.id, "amount": p.amount, "currency": "USD"}

	case "click":
		return map[string]any{
			"class": []string{"btn-primary", "btn-secondary", "btn-link", "card"}[rand.IntN(4)],
			"id":    fmt.Sprintf("el-%04d", rand.IntN(200)),
			"tag":   cssTags[rand.IntN(len(cssTags))],
			"text":  clickTexts[rand.IntN(len(clickTexts))],
			"x":     rand.IntN(1920), "y": rand.IntN(1080),
		}

	case "rage_click":
		// Paws are not a precision instrument.
		elements := []string{"#checkout-submit", "#treat-dispenser-demo", "#add-to-cart"}
		return map[string]any{
			"click_count": 3 + rand.IntN(5),
			"element":     elements[rand.IntN(len(elements))],
			"x":           rand.IntN(1920), "y": rand.IntN(1080),
		}

	case "dead_click":
		elements := []string{"#apply-coupon", "#picture-of-ball-not-a-button"}
		return map[string]any{
			"element": elements[rand.IntN(len(elements))],
			"text":    []string{"Apply", "Continue", "Submit", ""}[rand.IntN(4)],
			"x":       rand.IntN(1920), "y": rand.IntN(1080),
		}

	case "scroll":
		percent := rand.IntN(101)
		return map[string]any{"percent": percent, "scroll_y": percent * 50}

	case "error_occurred":
		codes := []struct{ code, severity string }{
			{"payment_declined", "warning"},
			{"inventory_sync_timeout", "error"},
			{"search_timeout", "warning"},
			{"500", "error"},
			{"network", "warning"},
			{"zoomies_interrupt", "warning"},
		}
		c := codes[rand.IntN(len(codes))]
		return map[string]any{"error_code": c.code, "severity": c.severity, "unhandled": c.severity == "error"}

	case "app_install":
		src := map[string]string{"ios": "app_store", "android": "play_store"}[st.prof.platform]
		return map[string]any{"app_version": latestAppVersionAt(st.start), "install_source": src}

	case "app_crashed":
		msgs := []string{
			"TooManySquirrelsException: viewport overflow",
			"SIGSEGV in ImageDecoder",
			"OutOfTreatsError: allocation failed",
		}
		return map[string]any{"error_message": msgs[rand.IntN(len(msgs))]}

	default:
		// app_open, app_close, app_backgrounded, app_foregrounded, signup,
		// signin, signout, share, ... carry no custom properties.
		return map[string]any{}
	}
}

func (st *sessionState) pickProduct() *product {
	p := catalog[weightedIndex(catalogWeights)]
	st.product = &p
	if st.prof.platform == "web" {
		st.page = "/products/" + p.slug
		st.pageTitle = p.name + " — Pug & Pals"
	}
	return &p
}

func (st *sessionState) currentProduct() *product {
	if st.product == nil {
		return st.pickProduct()
	}
	return st.product
}

// ensureCart guarantees a non-empty cart for journeys that enter the funnel
// mid-way. A previously abandoned cart is adopted when one exists (push
// recovery and organic cart returns resume the cart the user actually left);
// otherwise a fresh one-line cart is created.
func (st *sessionState) ensureCart() {
	if len(st.cart) > 0 {
		return
	}
	if st.mem != nil {
		if cart, ok := st.mem.takeAbandonedCartBefore(st.start); ok {
			st.cart = cart
			return
		}
	}
	st.cart = append(st.cart, cartLine{p: *st.currentProduct(), qty: 1})
}

func (st *sessionState) ensureCartID() string {
	if st.cartID == "" {
		st.cartID = "cart-" + shortID()
	}
	return st.cartID
}

func (st *sessionState) cartTotal() float64 {
	var total float64
	for _, l := range st.cart {
		total += l.p.price * float64(l.qty)
	}
	return round2(total)
}

func (st *sessionState) cartCount() int {
	n := 0
	for _, l := range st.cart {
		n += l.qty
	}
	return n
}

func (st *sessionState) videoID() string {
	return "vid-" + st.currentProduct().id
}

// plan picks a Pug Club tier once per session so trial → subscription →
// invoice events agree on plan and amount.
func (st *sessionState) plan() clubPlan {
	if st.planIdx == 0 {
		weights := make([]int, len(clubPlans))
		for i, p := range clubPlans {
			weights[i] = p.weight
		}
		st.planIdx = weightedIndex(weights) + 1
	}
	return clubPlans[st.planIdx-1]
}
