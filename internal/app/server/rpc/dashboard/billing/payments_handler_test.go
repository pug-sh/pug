package billing

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/authn"
	"connectrpc.com/connect"

	"github.com/pug-sh/pug/internal/app/server/rpc"
	"github.com/pug-sh/pug/internal/apperr"
	corebilling "github.com/pug-sh/pug/internal/core/billing"
	billingv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/billing/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/testutil"
)

// The buyer, whose address pre-fills the provider's form. Supplied by the
// dashboard middleware in production; the handler is not in that chain here.
func buyerCtx(t *testing.T) context.Context {
	t.Helper()
	return authn.SetInfo(t.Context(), &rpc.Principal{
		AuthType: rpc.AuthTypeJWT,
		Customer: &dbread.Customer{
			ID: "cust0000000000000000", Email: "buyer@acme.com", DisplayName: "Ada Buyer",
		},
	})
}

// The handlers return an *apperr.Error; ErrorInterceptor is what turns it into a
// connect error on the wire, and it is not in this call path.
func appErr(t *testing.T, err error) *apperr.Error {
	t.Helper()
	var ae *apperr.Error
	if !errors.As(err, &ae) {
		t.Fatalf("err = %v (%T), want an *apperr.Error", err, err)
	}
	return ae
}

const (
	checkoutSessionID = "cs_abc"
	checkoutURL       = "https://pay.example/checkout/abc"
	portalURL         = "https://pay.example/portal/abc"
)

type stubProvider struct{ in *corebilling.CheckoutInput }

func (stubProvider) Name() string { return "stub" }

func (stubProvider) Verify(http.Header, []byte) (corebilling.Delivery, error) {
	return corebilling.Delivery{}, nil
}

func (stubProvider) CanVerify() bool { return true }

func (stubProvider) Normalize(corebilling.Delivery) (corebilling.SubscriptionEvent, error) {
	return corebilling.SubscriptionEvent{}, nil
}

func (p stubProvider) CreateCheckoutSession(
	_ context.Context, in corebilling.CheckoutInput,
) (string, string, error) {
	if p.in != nil {
		*p.in = in
	}
	return checkoutSessionID, checkoutURL, nil
}

func (stubProvider) CreatePortalSession(context.Context, string) (string, error) {
	return portalURL, nil
}

func (stubProvider) FetchSubscription(context.Context, string) (corebilling.SubscriptionEvent, error) {
	return corebilling.SubscriptionEvent{}, nil
}

func (stubProvider) FetchCheckoutOutcome(context.Context, string) (corebilling.SubscriptionEvent, error) {
	return corebilling.SubscriptionEvent{}, nil
}

func (stubProvider) NormalizePayment(corebilling.Delivery) (corebilling.PaymentEvent, error) {
	return corebilling.PaymentEvent{}, nil
}

func (stubProvider) Charge(context.Context, corebilling.ChargeInput) (string, error) {
	return "pay_stub", nil
}

func (stubProvider) ListPayments(context.Context, string, time.Time) ([]corebilling.PaymentRecord, error) {
	return nil, nil
}

func (stubProvider) FetchPayment(context.Context, string) (corebilling.PaymentRecord, error) {
	return corebilling.PaymentRecord{}, nil
}

func (stubProvider) SetNextBillingDate(context.Context, string, time.Time) error { return nil }

func (stubProvider) CancelSubscription(context.Context, string) error { return nil }

// seedCustomer stands in for a completed checkout: the portal needs a customer.
func seedCustomer(t *testing.T, pg *testutil.TestPostgres, orgID string) {
	t.Helper()
	if _, err := pg.PgW.Exec(t.Context(),
		`insert into billing_subscriptions (
		   currency, id, on_demand, org_id, plan_slug, price_cents, provider,
		   provider_customer_id, provider_status, provider_sub_id, provider_updated_at, status)
		 values ('USD', 'sub00000000000000009', true, $1, 'usage-2026-09', 0, 'stub',
		         'cus_1', 'active', 'psub_9', now(), 'active')`, orgID); err != nil {
		t.Fatalf("seed subscription: %v", err)
	}
}

// failingProvider is the provider being down, with a message of its own.
type failingProvider struct{ stubProvider }

var errProviderDown = errors.New("dodo: 503 Service Unavailable (request id 7f3c)")

func (failingProvider) CreateCheckoutSession(context.Context, corebilling.CheckoutInput) (string, string, error) {
	return "", "", errProviderDown
}

func (failingProvider) CreatePortalSession(context.Context, string) (string, error) {
	return "", errProviderDown
}

func (failingProvider) FetchCheckoutOutcome(context.Context, string) (corebilling.SubscriptionEvent, error) {
	return corebilling.SubscriptionEvent{}, errProviderDown
}

func newFailingServer(t *testing.T, pg *testutil.TestPostgres) *Server {
	t.Helper()
	svc, err := corebilling.NewService(pg.PgRO, pg.PgW, corebilling.Config{Enabled: true}, &corebilling.Payments{
		MandateProduct: "prod_mandate",
		Provider:       failingProvider{},
		ReturnURL:      "https://app.example/settings/billing",
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return NewServer(svc)
}

// Internal, and silent: the provider's request ids and status text must not reach
// an API consumer.
func TestProviderFailuresAreInternalAndSayNothing(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	srv := newFailingServer(t, pg)
	orgID := seedOrg(t, pg, time.Now().AddDate(0, -6, 0))
	seedCustomer(t, pg, orgID)

	slug := corebilling.CurrentSlug
	sessionID := checkoutSessionID
	calls := map[string]func() error{
		"CreateCheckoutSession": func() error {
			_, err := srv.CreateCheckoutSession(buyerCtx(t), connect.NewRequest(
				&billingv1.CreateCheckoutSessionRequest{OrgId: &orgID, PlanSlug: &slug}))
			return err
		},
		"CreatePortalSession": func() error {
			_, err := srv.CreatePortalSession(t.Context(), connect.NewRequest(
				&billingv1.CreatePortalSessionRequest{OrgId: &orgID}))
			return err
		},
		"ConfirmCheckout": func() error {
			_, err := srv.ConfirmCheckout(t.Context(), connect.NewRequest(
				&billingv1.ConfirmCheckoutRequest{OrgId: &orgID, SessionId: &sessionID}))
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			err := call()
			if err == nil {
				t.Fatal("a provider outage was served as a success")
			}
			if got := connect.CodeOf(err); got != connect.CodeInternal {
				t.Errorf("code = %s, want INTERNAL", got)
			}
			if strings.Contains(err.Error(), "7f3c") || strings.Contains(err.Error(), "dodo") {
				t.Errorf("err = %q; it carries the provider's own message", err)
			}
		})
	}
}

// A provider that is fully configured except for the mandate product — the deploy
// variable is missing. Nothing is purchasable.
func newProductlessServer(t *testing.T, pg *testutil.TestPostgres) *Server {
	t.Helper()
	svc, err := corebilling.NewService(pg.PgRO, pg.PgW, corebilling.Config{Enabled: true}, &corebilling.Payments{
		Provider:  stubProvider{},
		ReturnURL: "https://app.example/settings/billing",
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return NewServer(svc)
}

func newPayingServer(t *testing.T, pg *testutil.TestPostgres, billingEnabled bool) *Server {
	t.Helper()
	svc, err := corebilling.NewService(pg.PgRO, pg.PgW, corebilling.Config{Enabled: billingEnabled}, &corebilling.Payments{
		MandateProduct: "prod_mandate",
		Provider:       stubProvider{},
		ReturnURL:      "https://app.example/settings/billing",
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return NewServer(svc)
}

func TestCheckoutPrefillsTheBuyer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	var in corebilling.CheckoutInput
	svc, err := corebilling.NewService(pg.PgRO, pg.PgW, corebilling.Config{Enabled: true}, &corebilling.Payments{
		MandateProduct: "prod_mandate",
		Provider:       stubProvider{in: &in},
		ReturnURL:      "https://app.example/settings/billing",
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	orgID := seedOrg(t, pg, time.Now().AddDate(0, -6, 0))
	if _, err := checkout(t, NewServer(svc), orgID, corebilling.CurrentSlug); err != nil {
		t.Fatalf("CreateCheckoutSession: %v", err)
	}
	// Both halves are strings, so a transposed pair compiles and reaches the provider.
	if in.CustomerEmail != "buyer@acme.com" {
		t.Errorf("CustomerEmail = %q, want buyer@acme.com", in.CustomerEmail)
	}
	if in.CustomerName != "Ada Buyer" {
		t.Errorf("CustomerName = %q, want Ada Buyer", in.CustomerName)
	}
	if in.ProductID != "prod_mandate" {
		t.Errorf("ProductID = %q, want the one mandate product", in.ProductID)
	}
}

func checkout(t *testing.T, srv *Server, orgID, slug string) (string, error) {
	t.Helper()
	resp, err := srv.CreateCheckoutSession(buyerCtx(t),
		connect.NewRequest(&billingv1.CreateCheckoutSessionRequest{OrgId: &orgID, PlanSlug: &slug}))
	if err != nil {
		return "", err
	}
	return resp.Msg.GetCheckoutUrl(), nil
}

// purchasable is what the dashboard renders the buy button from, and it must agree
// with what CreateCheckoutSession does — a button that cannot work is worse than
// none. Both read one helper; this asserts they agree in every configuration.
func TestPurchasableAgreesWithCheckout(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)

	cases := []struct {
		name    string
		server  func() *Server
		enabled bool
		want    bool
	}{
		{"provider configured", func() *Server { return newPayingServer(t, pg, true) }, true, true},
		{"no provider", func() *Server { return newServer(t, pg, true) }, true, false},
		{"billing off", func() *Server { return newPayingServer(t, pg, false) }, false, false},
		// A provider with credentials but no product ids: nothing is on sale, so
		// the button must not render even though checkout is otherwise wired.
		{"no products configured", func() *Server { return newProductlessServer(t, pg) }, true, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orgID := seedOrg(t, pg, time.Now().AddDate(0, -6, 0))
			srv := tc.server()

			got := getStatus(t, srv, orgID).GetPurchasable()
			if got != tc.want {
				t.Errorf("purchasable = %v, want %v", got, tc.want)
			}

			_, err := checkout(t, srv, orgID, corebilling.CurrentSlug)
			if tc.want && err != nil {
				t.Errorf("purchasable is true but checkout failed: %v", err)
			}
			if !tc.want && err == nil {
				t.Error("purchasable is false but checkout succeeded")
			}
		})
	}
}

func TestCheckoutReturnsAURL(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	orgID := seedOrg(t, pg, time.Now().AddDate(0, -6, 0))

	orgIDCopy, slug := orgID, corebilling.CurrentSlug
	resp, err := newPayingServer(t, pg, true).CreateCheckoutSession(buyerCtx(t),
		connect.NewRequest(&billingv1.CreateCheckoutSessionRequest{OrgId: &orgIDCopy, PlanSlug: &slug}))
	if err != nil {
		t.Fatalf("CreateCheckoutSession: %v", err)
	}
	if got := resp.Msg.GetCheckoutUrl(); got != checkoutURL {
		t.Errorf("checkout_url = %q, want %q", got, checkoutURL)
	}
	// Without this the buyer confirms with "", which min_len rejects — the feature
	// dies silently and every other assertion here still passes.
	if got := resp.Msg.GetSessionId(); got != checkoutSessionID {
		t.Errorf("session_id = %q, want %q", got, checkoutSessionID)
	}
}

func TestCheckoutRefusesWhatCannotBeSold(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	orgID := seedOrg(t, pg, time.Now().AddDate(0, -6, 0))
	srv := newPayingServer(t, pg, true)

	cases := map[string]struct {
		slug string
		code connect.Code
	}{
		// A floor is never sold.
		"free floor":  {corebilling.SlugFree, connect.CodeFailedPrecondition},
		"trial floor": {corebilling.SlugTrial, connect.CodeFailedPrecondition},
		// A negotiated deal with no terms recorded on the org.
		"custom with no deal": {corebilling.SlugCustom, connect.CodeFailedPrecondition},
		"no such plan":        {"platinum", connect.CodeNotFound},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := checkout(t, srv, orgID, tc.slug)
			if err == nil {
				t.Fatalf("checkout for %q succeeded", tc.slug)
			}
			if got := appErr(t, err).Code(); got != tc.code {
				t.Errorf("code = %s, want %s", got, tc.code)
			}
		})
	}
}

// A deployment with no provider is the self-hosted shape: quotas and grants
// work, and checkout is Unavailable rather than an error a user can act on.
func TestCheckoutIsUnavailableWithoutAProvider(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	orgID := seedOrg(t, pg, time.Now().AddDate(0, -6, 0))

	_, err := checkout(t, newServer(t, pg, true), orgID, corebilling.CurrentSlug)
	if err == nil {
		t.Fatal("checkout succeeded with no provider configured")
	}
	ae := appErr(t, err)
	if ae.Code() != connect.CodeUnavailable {
		t.Errorf("code = %s, want Unavailable", ae.Code())
	}
	if ae.Reason() != apperr.ReasonBillingUnavailable {
		t.Errorf("reason = %q, want BILLING_UNAVAILABLE", ae.Reason())
	}
}

// manageable is not implied by subscription_status: a cancelled org reports
// UNSPECIFIED and still has invoices to fetch and a card to re-add.
func TestPortalRequiresACustomer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	orgID := seedOrg(t, pg, time.Now().AddDate(0, -6, 0))
	srv := newPayingServer(t, pg, true)

	if getStatus(t, srv, orgID).GetManageable() {
		t.Error("manageable is true for an org that has never checked out")
	}
	_, err := srv.CreatePortalSession(buyerCtx(t),
		connect.NewRequest(&billingv1.CreatePortalSessionRequest{OrgId: &orgID}))
	if err == nil {
		t.Fatal("a portal session opened for an org with no customer")
	}
	if got := appErr(t, err).Code(); got != connect.CodeFailedPrecondition {
		t.Errorf("code = %s, want FailedPrecondition", got)
	}
}

// The case manageable exists for: the subscription is gone, so subscription_status
// reports UNSPECIFIED, but the customer remains with invoices to fetch. Inferring
// the button from the status would hide the portal from the org that wants it.
func TestCancelledOrgIsStillManageable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	orgID := seedOrg(t, pg, time.Now().AddDate(0, -6, 0))
	srv := newPayingServer(t, pg, true)

	if _, err := pg.PgW.Exec(t.Context(),
		`insert into billing_subscriptions (
		   currency, id, on_demand, org_id, plan_slug, price_cents, provider,
		   provider_customer_id, provider_status, provider_sub_id, provider_updated_at, status)
		 values ('USD', 'sub00000000000000001', true, $1, 'usage-2026-09', 0, 'stub',
		         'cus_1', 'cancelled', 'psub_1', now(), 'cancelled')`, orgID); err != nil {
		t.Fatalf("seed subscription: %v", err)
	}

	status := getStatus(t, srv, orgID)
	if status.GetSubscriptionStatus() != billingv1.SubscriptionStatus_SUBSCRIPTION_STATUS_UNSPECIFIED {
		t.Errorf("subscription_status = %s, want UNSPECIFIED — a dead subscription supplies nothing",
			status.GetSubscriptionStatus())
	}
	if status.GetPlan().GetSlug() != corebilling.CurrentSlug || status.GetChargeable() {
		t.Errorf("plan = %q chargeable=%v, want the current card, not chargeable", status.GetPlan().GetSlug(), status.GetChargeable())
	}
	if !status.GetManageable() {
		t.Error("manageable is false for a cancelled org that still has a customer at the provider")
	}
	if _, err := srv.CreatePortalSession(buyerCtx(t),
		connect.NewRequest(&billingv1.CreatePortalSessionRequest{OrgId: &orgID})); err != nil {
		t.Errorf("manageable is true but the portal refused: %v", err)
	}
}

// A checkout leaves a customer behind, and everything the dashboard needs to
// render the two buttons then follows from the same read.
func TestStatusReportsALiveSubscription(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	orgID := seedOrg(t, pg, time.Now().AddDate(0, -6, 0))
	srv := newPayingServer(t, pg, true)

	periodEnd := time.Now().Add(20 * 24 * time.Hour).UTC().Truncate(time.Second)
	if _, err := pg.PgW.Exec(t.Context(),
		`insert into billing_subscriptions (
		   currency, current_period_end, id, on_demand, org_id, plan_slug, price_cents, provider,
		   provider_customer_id, provider_status, provider_sub_id, provider_updated_at, status)
		 values ('USD', $1, 'sub00000000000000000', true, $2, 'usage-2026-09', 0, 'stub',
		         'cus_1', 'on_hold', 'psub_1', now(), 'past_due')`,
		periodEnd, orgID); err != nil {
		t.Fatalf("seed subscription: %v", err)
	}

	status := getStatus(t, srv, orgID)
	if status.GetSubscriptionStatus() != billingv1.SubscriptionStatus_SUBSCRIPTION_STATUS_PAST_DUE {
		t.Errorf("subscription_status = %s, want PAST_DUE", status.GetSubscriptionStatus())
	}
	// past_due keeps the mandate: a failed card is worth a banner, never a block.
	if status.GetPlan().GetSlug() != corebilling.CurrentSlug || !status.GetChargeable() {
		t.Errorf("plan = %q chargeable=%v, want the pinned card, chargeable", status.GetPlan().GetSlug(), status.GetChargeable())
	}
	if status.GetRateCard() == nil || status.GetRateCard().GetFreeBlocks() != 1 || len(status.GetRateCard().GetTiers()) != 3 {
		t.Errorf("rate_card = %v, want the card's tiers on the wire", status.GetRateCard())
	}
	if status.GetNextChargeAt() == nil {
		t.Error("next_charge_at is absent for a chargeable org")
	}
	if !status.GetManageable() {
		t.Error("manageable is false for an org with a provider customer")
	}
	if !status.GetCurrentPeriodEnd().AsTime().Equal(periodEnd) {
		t.Errorf("current_period_end = %s, want %s", status.GetCurrentPeriodEnd().AsTime(), periodEnd)
	}
	// The money's period is not the quota's window, and conflating them is the
	// mistake this pair of fields exists to prevent.
	if status.GetPeriodEnd().AsTime().Equal(periodEnd) {
		t.Error("period_end equals current_period_end; they answer different questions")
	}

	resp, err := srv.CreatePortalSession(buyerCtx(t),
		connect.NewRequest(&billingv1.CreatePortalSessionRequest{OrgId: &orgID}))
	if err != nil {
		t.Fatalf("CreatePortalSession: %v", err)
	}
	if resp.Msg.GetPortalUrl() != portalURL {
		t.Errorf("portal_url = %q, want %q", resp.Msg.GetPortalUrl(), portalURL)
	}
}

func listPlans(t *testing.T, srv *Server, orgID string) []*billingv1.PlanOption {
	t.Helper()
	resp, err := srv.ListPlans(t.Context(), connect.NewRequest(&billingv1.ListPlansRequest{OrgId: &orgID}))
	if err != nil {
		t.Fatalf("ListPlans: %v", err)
	}
	return resp.Msg.GetPlans()
}

// The catalog is one card, priced on the wire, plus custom for the org whose row
// records a deal — never the floors, never a product id.
func TestListPlansOffersTheCurrentCard(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	orgID := seedOrg(t, pg, time.Now().AddDate(0, -6, 0))
	srv := newPayingServer(t, pg, true)

	plans := listPlans(t, srv, orgID)
	if len(plans) != 1 {
		t.Fatalf("got %d plans, want just the current card: %+v", len(plans), plans)
	}
	card := plans[0]
	if card.GetSlug() != corebilling.CurrentSlug || !card.GetPurchasable() {
		t.Errorf("plan = %q purchasable=%v, want the current card, purchasable", card.GetSlug(), card.GetPurchasable())
	}
	if card.GetPriceCents() != nil {
		t.Errorf("price = %v, want absent — a card has no list price", card.GetPriceCents())
	}
	if card.GetRateCard() == nil || card.GetRateCard().GetBlockEvents() != 100_000 || card.GetRateCard().GetFreeBlocks() != 1 {
		t.Errorf("rate_card = %v", card.GetRateCard())
	}
	if got := card.GetIncludedEvents(); got == nil || got.GetValue() != 100_000 {
		t.Errorf("included events = %v, want the free allowance", got)
	}
	if got := card.GetRetentionDays(); got == nil || got.GetValue() != corebilling.RetentionDays {
		t.Errorf("retention = %v, want %d", got, corebilling.RetentionDays)
	}

	// With no mandate product nothing is purchasable, but the card still renders.
	plans = listPlans(t, newProductlessServer(t, pg), orgID)
	if len(plans) != 1 || plans[0].GetPurchasable() {
		t.Errorf("plans with no mandate product = %+v, want the card, not purchasable", plans)
	}
}

// A negotiated deal is buyable from the dashboard by the org whose row records
// its terms and by nobody else.
func TestListPlansOffersCustomOnlyToItsOwnOrg(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	orgID := seedOrg(t, pg, time.Now().AddDate(0, -6, 0))
	srv := newPayingServer(t, pg, true)

	if _, err := pg.PgW.Exec(t.Context(),
		`insert into billing_entitlements (block_rate_cents, included_events_override, org_id, plan_slug)
		 values (300, 5000000, $1, 'custom')`, orgID); err != nil {
		t.Fatalf("seed entitlement: %v", err)
	}

	var custom *billingv1.PlanOption
	for _, plan := range listPlans(t, srv, orgID) {
		if plan.GetSlug() == corebilling.SlugCustom {
			custom = plan
		}
	}
	if custom == nil {
		t.Fatal("custom is not offered to the org whose row records a deal")
	}
	if !custom.GetPurchasable() {
		t.Error("custom is not purchasable for the org that has a deal")
	}
	if custom.GetRateCard() != nil {
		t.Error("custom carries a rate card")
	}
	terms := custom.GetCustomTerms()
	if terms == nil || terms.GetBlockRateCents().GetValue() != 300 || terms.GetIncludedEvents().GetValue() != 5_000_000 {
		t.Errorf("custom terms = %v, want the deal's rate and allowance", terms)
	}
	if terms.GetFlatFeeCents() != nil {
		t.Errorf("flat fee = %v, want absent on a rate-only deal", terms.GetFlatFeeCents())
	}

	other := seedOrg(t, pg, time.Now().AddDate(0, -6, 0))
	for _, plan := range listPlans(t, srv, other) {
		if plan.GetSlug() == corebilling.SlugCustom {
			t.Error("custom is offered to an org with no deal")
		}
	}
}

// confirmStub answers FetchCheckoutOutcome with whatever a test needs, so the
// translations below run through the real handler rather than the sentinels.
type confirmStub struct {
	stubProvider
	event corebilling.SubscriptionEvent
	err   error
}

func (c confirmStub) FetchCheckoutOutcome(context.Context, string) (corebilling.SubscriptionEvent, error) {
	return c.event, c.err
}

func newConfirmingServer(t *testing.T, pg *testutil.TestPostgres, provider corebilling.PaymentProvider) *Server {
	t.Helper()
	svc, err := corebilling.NewService(pg.PgRO, pg.PgW, corebilling.Config{Enabled: true}, &corebilling.Payments{
		MandateProduct: "prod_mandate",
		Provider:       provider,
		ReturnURL:      "https://app.example/settings/billing",
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return NewServer(svc)
}

func confirm(t *testing.T, srv *Server, orgID string) (bool, error) {
	t.Helper()
	session := "cs_abc"
	resp, err := srv.ConfirmCheckout(buyerCtx(t),
		connect.NewRequest(&billingv1.ConfirmCheckoutRequest{OrgId: &orgID, SessionId: &session}))
	if err != nil {
		return false, err
	}
	return resp.Msg.GetConfirmed(), nil
}

// confirmRef is the ref pug would have minted for orgID's checkout.
func confirmRef(orgID string) string { return "ref_" + orgID }

// seedCheckoutRef stands in for the row CreateCheckoutSession writes.
func seedCheckoutRef(t *testing.T, pg *testutil.TestPostgres, orgID string) {
	t.Helper()
	if _, err := pg.PgW.Exec(t.Context(),
		`insert into billing_checkout_sessions (org_id, plan_slug, provider, ref) values ($1, $2, $3, $4)`,
		orgID, corebilling.CurrentSlug, stubProvider{}.Name(), confirmRef(orgID)); err != nil {
		t.Fatalf("seed checkout session: %v", err)
	}
}

func confirmEvent(orgID, subID, product string, status corebilling.SubStatus) corebilling.SubscriptionEvent {
	return corebilling.SubscriptionEvent{
		CheckoutRef:        confirmRef(orgID),
		Currency:           "USD",
		CurrentPeriodEnd:   time.Now().Add(20 * 24 * time.Hour),
		CurrentPeriodStart: time.Now().Add(-10 * 24 * time.Hour),
		OnDemand:           true,
		OrgID:              orgID,
		ProductID:          product,
		ProviderCustomerID: "cus_" + orgID,
		ProviderStatus:     string(status),
		ProviderSubID:      subID,
		Status:             status,
	}
}

func TestConfirmReportsASettledCheckout(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	orgID := seedOrg(t, pg, time.Now().AddDate(0, -6, 0))
	seedCheckoutRef(t, pg, orgID)

	srv := newConfirmingServer(t, pg, confirmStub{
		event: confirmEvent(orgID, "sub00000000000000040", "prod_mandate", corebilling.SubStatusActive),
	})
	confirmed, err := confirm(t, srv, orgID)
	if err != nil {
		t.Fatalf("ConfirmCheckout: %v", err)
	}
	if !confirmed {
		t.Error("confirmed = false for a settled checkout")
	}
}

// Every refusal that follows took the customer's money except the last, so none
// may reach them as "internal error" or as a plan that "cannot be purchased".
func TestConfirmTranslatesItsRefusals(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)

	cases := []struct {
		name       string
		stub       func(orgID string) confirmStub
		wantCode   connect.Code
		wantReason apperr.Reason
	}{
		{
			"another org's session",
			func(string) confirmStub {
				return confirmStub{event: confirmEvent("org-elsewhere", "sub00000000000000041", "prod_mandate", corebilling.SubStatusActive)}
			},
			connect.CodePermissionDenied, apperr.ReasonBillingCheckoutNotForOrg,
		},
		{
			"foreign currency",
			func(orgID string) confirmStub {
				e := confirmEvent(orgID, "sub00000000000000042", "prod_mandate", corebilling.SubStatusActive)
				e.Currency = "EUR"
				return confirmStub{event: e}
			},
			connect.CodeFailedPrecondition, apperr.ReasonBillingCurrencyUnsupported,
		},
		{
			// A recurring subscription would be billed by the provider AND by pug.
			"recurring subscription",
			func(orgID string) confirmStub {
				e := confirmEvent(orgID, "sub00000000000000043", "prod_mandate", corebilling.SubStatusActive)
				e.OnDemand = false
				return confirmStub{event: e}
			},
			connect.CodeFailedPrecondition, apperr.ReasonBillingSubscriptionUnapplicable,
		},
		{
			// Neither is pre-checked by ConfirmCheckout, so both reach the writer's guard.
			"no customer on the provider's record",
			func(orgID string) confirmStub {
				e := confirmEvent(orgID, "sub00000000000000044", "prod_mandate", corebilling.SubStatusActive)
				e.ProviderCustomerID = ""
				return confirmStub{event: e}
			},
			connect.CodeFailedPrecondition, apperr.ReasonBillingSubscriptionUnapplicable,
		},
		{
			"no status on the provider's record",
			func(orgID string) confirmStub {
				e := confirmEvent(orgID, "sub00000000000000045", "prod_mandate", corebilling.SubStatusActive)
				e.Status = ""
				return confirmStub{event: e}
			},
			connect.CodeFailedPrecondition, apperr.ReasonBillingSubscriptionUnapplicable,
		},
		{
			"declined card",
			func(string) confirmStub { return confirmStub{err: corebilling.ErrCheckoutFailed} },
			connect.CodeFailedPrecondition, apperr.ReasonBillingCheckoutFailed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orgID := seedOrg(t, pg, time.Now().AddDate(0, -6, 0))
			seedCheckoutRef(t, pg, orgID)
			_, err := confirm(t, newConfirmingServer(t, pg, tc.stub(orgID)), orgID)
			if err == nil {
				t.Fatal("err = nil, want a refusal")
			}
			ae := appErr(t, err)
			if ae.Code() != tc.wantCode {
				t.Errorf("code = %v, want %v", ae.Code(), tc.wantCode)
			}
			if ae.Reason() != tc.wantReason {
				t.Errorf("reason = %q, want %q", ae.Reason(), tc.wantReason)
			}
		})
	}
}

// The org already holds a live subscription, so the paid one cannot be written.
// Its own reason, because "internal error" is what this used to be.
func TestConfirmRefusesASecondLiveSubscription(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	orgID := seedOrg(t, pg, time.Now().AddDate(0, -6, 0))
	seedCheckoutRef(t, pg, orgID)

	live := confirmEvent(orgID, "sub00000000000000044", "prod_mandate", corebilling.SubStatusActive)
	if _, err := confirm(t, newConfirmingServer(t, pg, confirmStub{event: live}), orgID); err != nil {
		t.Fatalf("seed confirm: %v", err)
	}

	second := confirmEvent(orgID, "sub00000000000000045", "prod_mandate", corebilling.SubStatusActive)
	_, err := confirm(t, newConfirmingServer(t, pg, confirmStub{event: second}), orgID)
	if err == nil {
		t.Fatal("err = nil for a second live subscription, want a refusal")
	}
	ae := appErr(t, err)
	if ae.Code() != connect.CodeFailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition", ae.Code())
	}
	if ae.Reason() != apperr.ReasonBillingTwoLiveSubscriptions {
		t.Errorf("reason = %q, want %q", ae.Reason(), apperr.ReasonBillingTwoLiveSubscriptions)
	}
}
