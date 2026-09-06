package billing

import (
	"context"
	"errors"
	"net/http"
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
		Customer: &dbread.Customer{ID: "cust0000000000000000", Email: "buyer@acme.com"},
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
	checkoutURL = "https://pay.example/checkout/abc"
	portalURL   = "https://pay.example/portal/abc"
)

type stubProvider struct{}

func (stubProvider) Name() string { return "stub" }

func (stubProvider) Verify(http.Header, []byte) (corebilling.Delivery, error) {
	return corebilling.Delivery{}, nil
}

func (stubProvider) Normalize(corebilling.Delivery) (corebilling.SubscriptionEvent, error) {
	return corebilling.SubscriptionEvent{}, nil
}

func (stubProvider) CreateCheckoutSession(context.Context, corebilling.CheckoutInput) (string, error) {
	return checkoutURL, nil
}

func (stubProvider) CreatePortalSession(context.Context, string) (string, error) {
	return portalURL, nil
}

func (stubProvider) FetchSubscription(context.Context, string) (corebilling.SubscriptionEvent, error) {
	return corebilling.SubscriptionEvent{}, nil
}

// A provider that is fully configured except that no catalog tier has a product
// id -- the deploy variable is missing. Nothing is purchasable.
func newProductlessServer(t *testing.T, pg *testutil.TestPostgres) *Server {
	t.Helper()
	svc, err := corebilling.NewService(pg.PgRO, pg.PgW, true, &corebilling.Payments{
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
	svc, err := corebilling.NewService(pg.PgRO, pg.PgW, billingEnabled, &corebilling.Payments{
		ProductBySlug: map[string]string{"growth": "prod_growth"},
		Provider:      stubProvider{},
		ReturnURL:     "https://app.example/settings/billing",
		SlugByProduct: map[string]string{"prod_growth": "growth"},
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return NewServer(svc)
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

// purchasable is what the dashboard renders the buy button from, and it must
// agree with what CreateCheckoutSession actually does -- a button that cannot
// work is worse than no button. Both read one helper, and this asserts they
// agree in every configuration.
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

			_, err := checkout(t, srv, orgID, "growth")
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

	url, err := checkout(t, newPayingServer(t, pg, true), orgID, "growth")
	if err != nil {
		t.Fatalf("CreateCheckoutSession: %v", err)
	}
	if url != checkoutURL {
		t.Errorf("checkout_url = %q, want %q", url, checkoutURL)
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
		// A floor is never sold, even though the catalog knows it.
		"free floor":  {corebilling.SlugFree, connect.CodeFailedPrecondition},
		"trial floor": {corebilling.SlugTrial, connect.CodeFailedPrecondition},
		// Configured in the catalog but with no product id in this deployment.
		"unconfigured tier": {"scale", connect.CodeFailedPrecondition},
		// A negotiated deal with no product id recorded on the org.
		"custom with no product": {corebilling.SlugCustom, connect.CodeFailedPrecondition},
		"no such plan":           {"platinum", connect.CodeNotFound},
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

	_, err := checkout(t, newServer(t, pg, true), orgID, "growth")
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

// The case manageable exists for: the subscription is gone, so
// subscription_status reports UNSPECIFIED, but the customer remains and still
// has invoices to fetch and a card to re-add. Inferring the button from the
// status would hide the portal from exactly the org most likely to want it.
func TestCancelledOrgIsStillManageable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	orgID := seedOrg(t, pg, time.Now().AddDate(0, -6, 0))
	srv := newPayingServer(t, pg, true)

	if _, err := pg.PgW.Exec(t.Context(),
		`insert into billing_subscriptions (
		   currency, id, org_id, plan_slug, price_cents, provider,
		   provider_customer_id, provider_status, provider_sub_id, provider_updated_at, status)
		 values ('USD', 'sub00000000000000001', $1, 'growth', 2000, 'stub',
		         'cus_1', 'cancelled', 'psub_1', now(), 'cancelled')`, orgID); err != nil {
		t.Fatalf("seed subscription: %v", err)
	}

	status := getStatus(t, srv, orgID)
	if status.GetSubscriptionStatus() != billingv1.SubscriptionStatus_SUBSCRIPTION_STATUS_UNSPECIFIED {
		t.Errorf("subscription_status = %s, want UNSPECIFIED — a dead subscription supplies nothing",
			status.GetSubscriptionStatus())
	}
	if status.GetPlan().GetSlug() != corebilling.SlugFree {
		t.Errorf("plan = %q, want free", status.GetPlan().GetSlug())
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
		   currency, current_period_end, id, org_id, plan_slug, price_cents, provider,
		   provider_customer_id, provider_status, provider_sub_id, provider_updated_at, status)
		 values ('USD', $1, 'sub00000000000000000', $2, 'growth', 2000, 'stub',
		         'cus_1', 'on_hold', 'psub_1', now(), 'past_due')`,
		periodEnd, orgID); err != nil {
		t.Fatalf("seed subscription: %v", err)
	}

	status := getStatus(t, srv, orgID)
	if status.GetSubscriptionStatus() != billingv1.SubscriptionStatus_SUBSCRIPTION_STATUS_PAST_DUE {
		t.Errorf("subscription_status = %s, want PAST_DUE", status.GetSubscriptionStatus())
	}
	// past_due keeps the plan: a failed card is worth a banner, never a block.
	if status.GetPlan().GetSlug() != "growth" {
		t.Errorf("plan = %q, want growth", status.GetPlan().GetSlug())
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

// The catalog is what the dashboard names a tier from, and it must never name
// something nobody can buy or something that is not for sale.
func TestListPlansOffersOnlySellableTiers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	orgID := seedOrg(t, pg, time.Now().AddDate(0, -6, 0))
	srv := newPayingServer(t, pg, true)

	bySlug := map[string]*billingv1.PlanOption{}
	for _, plan := range listPlans(t, srv, orgID) {
		bySlug[plan.GetSlug()] = plan
	}

	for _, floor := range []string{corebilling.SlugFree, corebilling.SlugTrial} {
		if _, ok := bySlug[floor]; ok {
			t.Errorf("%q is offered for sale; nobody buys a floor", floor)
		}
	}
	// No product id on this org's row, so there is no custom deal to buy.
	if _, ok := bySlug[corebilling.SlugCustom]; ok {
		t.Error("custom is offered to an org with no recorded product")
	}

	// Per tier, not per org: this deployment configured growth and not scale.
	if got := bySlug["growth"]; got == nil || !got.GetPurchasable() {
		t.Errorf("growth purchasable = %v, want true", got.GetPurchasable())
	}
	if got := bySlug["scale"]; got == nil || got.GetPurchasable() {
		t.Error("scale is purchasable with no product configured for it")
	}

	// The list price, not the org's negotiated anything.
	if got := bySlug["growth"].GetPriceCents(); got == nil || got.GetValue() != 2_000 {
		t.Errorf("growth price = %v, want 2000", got)
	}
	if got := bySlug["growth"].GetIncludedEvents(); got == nil || got.GetValue() != 500_000 {
		t.Errorf("growth quota = %v, want 500000", got)
	}
}

// A negotiated deal is buyable from the dashboard by the org whose row records
// its product, and by nobody else. That is what lets an operator paste one id
// instead of emailing a payment link.
func TestListPlansOffersCustomOnlyToItsOwnOrg(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	orgID := seedOrg(t, pg, time.Now().AddDate(0, -6, 0))
	srv := newPayingServer(t, pg, true)

	quota := int64(5_000_000)
	productID := "prod_acme"
	if _, err := pg.PgW.Exec(t.Context(),
		`insert into billing_entitlements (included_events_override, org_id, plan_slug, provider_product_id)
		 values ($1, $2, 'custom', $3)`, quota, orgID, productID); err != nil {
		t.Fatalf("seed entitlement: %v", err)
	}

	var custom *billingv1.PlanOption
	for _, plan := range listPlans(t, srv, orgID) {
		if plan.GetSlug() == corebilling.SlugCustom {
			custom = plan
		}
	}
	if custom == nil {
		t.Fatal("custom is not offered to the org whose row records its product")
	}
	if !custom.GetPurchasable() {
		t.Error("custom is not purchasable for the org that has its product id")
	}
	// The catalog price and quota, not this org's negotiated ones — those belong
	// to the entitlement, which GetBillingStatus reports.
	if custom.GetPriceCents() != nil {
		t.Errorf("custom price = %v, want absent — a deal's price lives in the provider", custom.GetPriceCents())
	}
	if custom.GetIncludedEvents() != nil {
		t.Errorf("custom quota = %v, want absent on the catalog entry", custom.GetIncludedEvents())
	}
}
