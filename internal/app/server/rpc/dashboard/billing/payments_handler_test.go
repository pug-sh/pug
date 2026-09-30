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
	"github.com/pug-sh/pug/internal/core/billing/entitlement"
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

// seedCustomer stands in for a completed checkout: the portal needs a customer.
func seedCustomer(t *testing.T, pg *testutil.TestPostgres, orgID string) {
	t.Helper()
	if _, err := pg.PgW.Exec(t.Context(),
		`insert into billing_subscriptions (
		   currency, id, org_id, plan_slug, price_cents, provider,
		   provider_customer_id, provider_status, provider_sub_id, provider_updated_at, status)
		 values ('USD', 'sub00000000000000009', $1, 'usage-2026-10', 100, 'stub',
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
	return newServerWith(t, pg, true, &corebilling.Payments{
		ProductBySlug: map[string]string{entitlement.SlugUsage: "prod_u"},
		Provider:      failingProvider{},
		ReturnURL:     "https://app.example/settings/billing",
	})
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

	slug := entitlement.SlugUsage
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

// A provider that is fully configured except that no catalog tier has a product
// id — the deploy variable is missing. Nothing is purchasable.
func newProductlessServer(t *testing.T, pg *testutil.TestPostgres) *Server {
	t.Helper()
	return newServerWith(t, pg, true, &corebilling.Payments{
		Provider:  stubProvider{},
		ReturnURL: "https://app.example/settings/billing",
	})
}

func newPayingServer(t *testing.T, pg *testutil.TestPostgres, billingEnabled bool) *Server {
	t.Helper()
	return newServerWith(t, pg, billingEnabled, &corebilling.Payments{
		ProductBySlug: map[string]string{entitlement.SlugUsage: "prod_u"},
		Provider:      stubProvider{},
		ReturnURL:     "https://app.example/settings/billing",
	})
}

func TestCheckoutPrefillsTheBuyer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	var in corebilling.CheckoutInput
	srv := newServerWith(t, pg, true, &corebilling.Payments{
		ProductBySlug: map[string]string{entitlement.SlugUsage: "prod_u"},
		Provider:      stubProvider{in: &in},
		ReturnURL:     "https://app.example/settings/billing",
	})
	orgID := seedOrg(t, pg, time.Now().AddDate(0, -6, 0))
	if _, err := checkout(t, srv, orgID, entitlement.SlugUsage); err != nil {
		t.Fatalf("CreateCheckoutSession: %v", err)
	}
	// Both halves are strings, so a transposed pair compiles and reaches the provider.
	if in.CustomerEmail != "buyer@acme.com" {
		t.Errorf("CustomerEmail = %q, want buyer@acme.com", in.CustomerEmail)
	}
	if in.CustomerName != "Ada Buyer" {
		t.Errorf("CustomerName = %q, want Ada Buyer", in.CustomerName)
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
		// A product only for a plan checkout will not sell: app/payments keeps a
		// retired plan mapped for its holders' renewals, so once the one plan a
		// deployment has a product for retires, the map is not empty and nothing is
		// on sale. Free, never sold, stands in for the retired plan the catalog lacks.
		{"only a plan off sale has a product", func() *Server {
			return newServerWith(t, pg, true, &corebilling.Payments{
				ProductBySlug: map[string]string{entitlement.SlugFree: "prod_free"},
				Provider:      stubProvider{},
				ReturnURL:     "https://app.example/settings/billing",
			})
		}, true, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orgID := seedOrg(t, pg, time.Now().AddDate(0, -6, 0))
			srv := tc.server()

			status := getStatus(t, srv, orgID)
			if got := status.GetBillingEnabled(); got != tc.enabled {
				t.Errorf("billing_enabled = %v, want %v", got, tc.enabled)
			}
			got := status.GetPurchasable()
			if got != tc.want {
				t.Errorf("purchasable = %v, want %v", got, tc.want)
			}

			_, err := checkout(t, srv, orgID, entitlement.SlugUsage)
			if tc.want && err != nil {
				t.Errorf("purchasable is true but checkout failed: %v", err)
			}
			if !tc.want && err == nil {
				t.Error("purchasable is false but checkout succeeded")
			}
		})
	}
}

// Custom is a state rather than a catalog plan, so a deal's product comes from the
// org's own row: an org holding one is purchasable where no catalog plan has a
// product, exactly as its checkout opens.
func TestPurchasableCountsTheOrgsOwnDeal(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	orgID := seedOrg(t, pg, time.Now().AddDate(0, -6, 0))
	srv := newProductlessServer(t, pg)
	if _, err := pg.PgW.Exec(t.Context(),
		`insert into billing_entitlements (org_id, plan_slug, provider_product_id)
		 values ($1, 'custom', 'prod_acme')`, orgID); err != nil {
		t.Fatalf("seed entitlement: %v", err)
	}

	if !getStatus(t, srv, orgID).GetPurchasable() {
		t.Error("purchasable = false for the org whose row records its deal's product")
	}
	if _, err := checkout(t, srv, orgID, entitlement.SlugCustom); err != nil {
		t.Errorf("checkout of the org's own deal: %v", err)
	}
}

func TestCheckoutReturnsAURL(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	orgID := seedOrg(t, pg, time.Now().AddDate(0, -6, 0))

	orgIDCopy, slug := orgID, entitlement.SlugUsage
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
	var in corebilling.CheckoutInput
	srv := newServerWith(t, pg, true, &corebilling.Payments{
		ProductBySlug: map[string]string{entitlement.SlugUsage: "prod_u"},
		Provider:      stubProvider{in: &in},
		ReturnURL:     "https://app.example/settings/billing",
	})

	cases := map[string]struct {
		slug string
		code connect.Code
	}{
		// Free is a state, never sold.
		"free": {entitlement.SlugFree, connect.CodeFailedPrecondition},
		// A negotiated deal with no product id recorded on the org.
		"custom with no product": {entitlement.SlugCustom, connect.CodeFailedPrecondition},
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
	// Refused before the provider opened anything, not after.
	if in.ProductID != "" {
		t.Errorf("the provider was asked to open a checkout for %q", in.ProductID)
	}

	// In the catalog but with no product id in this deployment.
	_, err := checkout(t, newProductlessServer(t, pg), orgID, entitlement.SlugUsage)
	if err == nil {
		t.Fatal("checkout for a plan with no product succeeded")
	}
	if got := appErr(t, err).Code(); got != connect.CodeFailedPrecondition {
		t.Errorf("unconfigured plan: code = %s, want FailedPrecondition", got)
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

	_, err := checkout(t, newServer(t, pg, true), orgID, entitlement.SlugUsage)
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

// manageable is what the dashboard renders the portal button from, and it must
// agree with CreatePortalSession as purchasable does with checkout. Billing off is
// the case to watch: the server builds payments whether or not the switch is on,
// so a provider and a customer can both exist behind a switched-off deployment.
func TestManageableAgreesWithThePortal(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	cases := []struct {
		name    string
		server  func(*testutil.TestPostgres) *Server
		enabled bool
		want    bool
	}{
		{"provider configured", func(pg *testutil.TestPostgres) *Server { return newPayingServer(t, pg, true) }, true, true},
		{"no provider", func(pg *testutil.TestPostgres) *Server { return newServer(t, pg, true) }, true, false},
		{"billing off", func(pg *testutil.TestPostgres) *Server { return newPayingServer(t, pg, false) }, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A database each: seedCustomer's subscription ids are fixed.
			pg := testutil.SetupPostgres(t)
			orgID := seedOrg(t, pg, time.Now().AddDate(0, -6, 0))
			seedCustomer(t, pg, orgID)
			srv := tc.server(pg)

			status := getStatus(t, srv, orgID)
			if got := status.GetBillingEnabled(); got != tc.enabled {
				t.Errorf("billing_enabled = %v, want %v", got, tc.enabled)
			}
			if got := status.GetManageable(); got != tc.want {
				t.Errorf("manageable = %v, want %v", got, tc.want)
			}

			_, err := srv.CreatePortalSession(buyerCtx(t),
				connect.NewRequest(&billingv1.CreatePortalSessionRequest{OrgId: &orgID}))
			if tc.want && err != nil {
				t.Errorf("manageable is true but the portal refused: %v", err)
			}
			if !tc.want && err == nil {
				t.Error("manageable is false but the portal opened")
			}
		})
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
		   currency, id, org_id, plan_slug, price_cents, provider,
		   provider_customer_id, provider_status, provider_sub_id, provider_updated_at, status)
		 values ('USD', 'sub00000000000000001', $1, 'usage-2026-10', 100, 'stub',
		         'cus_1', 'cancelled', 'psub_1', now(), 'cancelled')`, orgID); err != nil {
		t.Fatalf("seed subscription: %v", err)
	}

	status := getStatus(t, srv, orgID)
	if status.GetSubscriptionStatus() != billingv1.SubscriptionStatus_SUBSCRIPTION_STATUS_UNSPECIFIED {
		t.Errorf("subscription_status = %s, want UNSPECIFIED — a dead subscription supplies nothing",
			status.GetSubscriptionStatus())
	}
	if status.GetPlan().GetSlug() != entitlement.SlugFree {
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
		 values ('USD', $1, 'sub00000000000000000', $2, 'usage-2026-10', 100, 'stub',
		         'cus_1', 'on_hold', 'psub_1', now(), 'past_due')`,
		periodEnd, orgID); err != nil {
		t.Fatalf("seed subscription: %v", err)
	}

	status := getStatus(t, srv, orgID)
	if status.GetSubscriptionStatus() != billingv1.SubscriptionStatus_SUBSCRIPTION_STATUS_PAST_DUE {
		t.Errorf("subscription_status = %s, want PAST_DUE", status.GetSubscriptionStatus())
	}
	// past_due keeps the plan: a failed card is worth a banner, never a block.
	if status.GetPlan().GetSlug() != entitlement.SlugUsage {
		t.Errorf("plan = %q, want %q", status.GetPlan().GetSlug(), entitlement.SlugUsage)
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

	// Exactly the usage plan: free is a state nobody buys, and with no product id on
	// this org's row there is no custom deal to buy.
	if len(bySlug) != 1 || bySlug[entitlement.SlugUsage] == nil {
		t.Fatalf("plans = %v, want exactly %s", bySlug, entitlement.SlugUsage)
	}
	usage := bySlug[entitlement.SlugUsage]
	if !usage.GetPurchasable() {
		t.Error("the usage plan is not purchasable with its product configured")
	}
	// Quantities only: the allowance and the retention, never a price.
	plan := entitlement.CurrentPlan()
	if got := usage.GetIncludedEvents(); got == nil || got.GetValue() != plan.FreeEvents {
		t.Errorf("allowance = %v, want %d", got, plan.FreeEvents)
	}
	// A wrapper for the same reason the allowance is one: a pricing table rendering
	// "0 days of history" beside a plan is worse than rendering nothing.
	if got := usage.GetRetentionDays(); got == nil || got.GetValue() != plan.RetentionDays {
		t.Errorf("retention = %v, want %d", got, plan.RetentionDays)
	}

	// Per plan, not per org: a deployment with no product for it cannot sell it.
	for _, option := range listPlans(t, newProductlessServer(t, pg), orgID) {
		if option.GetSlug() == entitlement.SlugUsage && option.GetPurchasable() {
			t.Error("the usage plan is purchasable with no product configured for it")
		}
	}
}

// A negotiated deal is buyable from the dashboard by the org whose row records its
// product and by nobody else — one pasted id instead of an emailed payment link.
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
		if plan.GetSlug() == entitlement.SlugCustom {
			custom = plan
		}
	}
	if custom == nil {
		t.Fatal("custom is not offered to the org whose row records its product")
	}
	if !custom.GetPurchasable() {
		t.Error("custom is not purchasable for the org that has its product id")
	}
	// Not this org's negotiated terms — those belong to the entitlement, which
	// GetBillingStatus reports.
	if custom.GetIncludedEvents() != nil {
		t.Errorf("custom quota = %v, want absent on the catalog entry", custom.GetIncludedEvents())
	}
	if custom.GetRetentionDays() != nil {
		t.Errorf("custom retention = %v, want absent — a deal's term is on its own row", custom.GetRetentionDays())
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
	return newServerWith(t, pg, true, &corebilling.Payments{
		ProductBySlug: map[string]string{entitlement.SlugUsage: "prod_u"},
		Provider:      provider,
		ReturnURL:     "https://app.example/settings/billing",
	})
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
		`insert into billing_checkout_sessions (org_id, provider, ref) values ($1, $2, $3)`,
		orgID, stubProvider{}.Name(), confirmRef(orgID)); err != nil {
		t.Fatalf("seed checkout session: %v", err)
	}
}

func confirmEvent(orgID, subID, product string, status corebilling.SubStatus) corebilling.SubscriptionEvent {
	return corebilling.SubscriptionEvent{
		CheckoutRef:        confirmRef(orgID),
		Currency:           "USD",
		CurrentPeriodEnd:   time.Now().Add(20 * 24 * time.Hour),
		CurrentPeriodStart: time.Now().Add(-10 * 24 * time.Hour),
		OrgID:              orgID,
		PriceCents:         2_000,
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
		event: confirmEvent(orgID, "sub00000000000000040", "prod_u", corebilling.SubStatusActive),
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
				return confirmStub{event: confirmEvent("org-elsewhere", "sub00000000000000041", "prod_u", corebilling.SubStatusActive)}
			},
			connect.CodePermissionDenied, apperr.ReasonBillingCheckoutNotForOrg,
		},
		{
			"foreign currency",
			func(orgID string) confirmStub {
				e := confirmEvent(orgID, "sub00000000000000042", "prod_u", corebilling.SubStatusActive)
				e.Currency = "EUR"
				return confirmStub{event: e}
			},
			connect.CodeFailedPrecondition, apperr.ReasonBillingCurrencyUnsupported,
		},
		{
			// Not NOT_PURCHASABLE: that is a checkout refused before any money moved.
			"product pug cannot place",
			func(orgID string) confirmStub {
				return confirmStub{event: confirmEvent(orgID, "sub00000000000000043", "prod_unknown", corebilling.SubStatusActive)}
			},
			connect.CodeFailedPrecondition, apperr.ReasonBillingProductUnmapped,
		},
		{
			// Neither is pre-checked by ConfirmCheckout, so both reach the writer's guard.
			"no customer on the provider's record",
			func(orgID string) confirmStub {
				e := confirmEvent(orgID, "sub00000000000000044", "prod_u", corebilling.SubStatusActive)
				e.ProviderCustomerID = ""
				return confirmStub{event: e}
			},
			connect.CodeFailedPrecondition, apperr.ReasonBillingSubscriptionUnapplicable,
		},
		{
			"no status on the provider's record",
			func(orgID string) confirmStub {
				e := confirmEvent(orgID, "sub00000000000000045", "prod_u", corebilling.SubStatusActive)
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

	live := confirmEvent(orgID, "sub00000000000000044", "prod_u", corebilling.SubStatusActive)
	if _, err := confirm(t, newConfirmingServer(t, pg, confirmStub{event: live}), orgID); err != nil {
		t.Fatalf("seed confirm: %v", err)
	}

	second := confirmEvent(orgID, "sub00000000000000045", "prod_u", corebilling.SubStatusActive)
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
