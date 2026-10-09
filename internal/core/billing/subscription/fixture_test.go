package subscription_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
	"github.com/pug-sh/pug/internal/core/billing/entitlement"
	"github.com/pug-sh/pug/internal/core/billing/subscription"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
	"github.com/rs/xid"
)

const actor = "tester@localhost"

// The subscription tests drive both services: a checkout or a delivery is applied
// against an entitlement the test set up first.
type fixture struct {
	svc          *subscription.Service
	entitlements *entitlement.Service
	pg           *testutil.TestPostgres
	orgID        string
}

// newFixture is billing on with no provider; newPaidFixture adds one.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	pg := testutil.SetupPostgres(t)

	orgID := dbwriteOrg(t, pg)
	entitlements, err := entitlement.NewService(pg.PgRO, pg.PgW, corebilling.Config{Enabled: true})
	if err != nil {
		t.Fatalf("new entitlement service: %v", err)
	}
	return &fixture{
		svc:          subscription.NewService(pg.PgRO, pg.PgW, nil, entitlements),
		entitlements: entitlements,
		pg:           pg,
		orgID:        orgID,
	}
}

// dbwriteOrg creates an org backdated to a fixed date, so the anchor and every
// period derived from it do not depend on the day the suite runs.
func dbwriteOrg(t *testing.T, pg *testutil.TestPostgres) string {
	t.Helper()
	org, err := dbwrite.New(pg.PgW).CreateOrg(t.Context(), dbwrite.CreateOrgParams{
		ID:          xid.New().String(),
		DisplayName: "acme",
	})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	testutil.SetOrgCreateTime(t, pg.PgW, org.ID, time.Date(2025, 3, 10, 0, 0, 0, 0, time.UTC))
	return org.ID
}

// svcWithMeter is a service whose provider also meters usage.
func (f *fixture) svcWithMeter(t *testing.T, provider corebilling.PaymentProvider, meter corebilling.UsageMeter) *subscription.Service {
	t.Helper()
	return subscription.NewService(f.pg.PgRO, f.pg.PgW, &corebilling.Payments{
		Provider: provider, ProductBySlug: map[string]string{}, Usage: meter,
	}, f.entitlements)
}

// svcWithProvider builds the paid service against provider, sharing the fixture's
// pools and entitlement service, so a test that swaps the provider still sees the
// rows it seeded.
func (f *fixture) svcWithProvider(t *testing.T, provider corebilling.PaymentProvider) *subscription.Service {
	t.Helper()
	return subscription.NewService(f.pg.PgRO, f.pg.PgW, &corebilling.Payments{
		ProductBySlug: map[string]string{entitlement.SlugUsage: "prod_u"},
		Provider:      provider,
		ReturnURL:     "https://app.example/settings/billing",
	}, f.entitlements)
}

// fakeProvider is the whole seam, stubbed: the inbox, the CAS, attribution and the
// rejection dispositions are all exercised through it, which is what proves those
// paths hold no Dodo assumption.
type fakeProvider struct {
	name  string
	event corebilling.SubscriptionEvent
	err   error
	// The confirm-on-return path reads its own event, so a test can make the
	// provider disagree with what any delivery said.
	checkout    corebilling.SubscriptionEvent
	checkoutErr error
	// checkoutIn is the last input CreateCheckoutSession was handed, so a test can
	// join the ref pug stored against the ref it sent.
	checkoutIn corebilling.CheckoutInput
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) Verify(http.Header, []byte) (corebilling.Delivery, error) {
	return corebilling.Delivery{}, nil
}

func (f *fakeProvider) CanVerify() bool { return true }

func (f *fakeProvider) Normalize(corebilling.Delivery) (corebilling.SubscriptionEvent, error) {
	return f.event, f.err
}

func (f *fakeProvider) CreateCheckoutSession(
	_ context.Context, in corebilling.CheckoutInput,
) (string, string, error) {
	f.checkoutIn = in
	return "cs_fake", "https://pay.example/checkout", nil
}

func (f *fakeProvider) CreatePortalSession(context.Context, string) (string, error) {
	return "https://pay.example/portal", nil
}

func (f *fakeProvider) FetchSubscription(context.Context, string) (corebilling.SubscriptionEvent, error) {
	return f.event, nil
}

func (f *fakeProvider) FetchCheckoutOutcome(context.Context, string) (corebilling.SubscriptionEvent, error) {
	return f.checkout, f.checkoutErr
}

const fakeProviderName = "fake"

func newPaidFixture(t *testing.T) (*fixture, *fakeProvider) {
	t.Helper()
	f := newFixture(t)
	provider := &fakeProvider{name: fakeProviderName}
	f.svc = f.svcWithProvider(t, provider)
	seedCheckoutRef(t, f, f.orgID)
	return f, provider
}

// checkoutRef is the ref a delivery for orgID carries. Attribution is by ref, so a
// test org needs a stored checkout to be reachable at all.
func checkoutRef(orgID string) string {
	if orgID == "" {
		return ""
	}
	return "ref_" + orgID
}

// seedCheckoutRef stands in for the row CreateCheckoutSession writes.
func seedCheckoutRef(t *testing.T, f *fixture, orgID string) {
	t.Helper()
	if _, err := f.pg.PgW.Exec(t.Context(),
		`insert into billing_checkout_sessions (org_id, provider, ref) values ($1, $2, $3)`,
		orgID, fakeProviderName, checkoutRef(orgID)); err != nil {
		t.Fatalf("seed checkout session: %v", err)
	}
}
