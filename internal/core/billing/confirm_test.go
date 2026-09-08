package billing_test

import (
	"errors"
	"testing"
	"time"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
)

// storedSubscriptions counts the rows the confirm path writes, so a refusal can
// be shown to have written nothing rather than merely to have returned an error.
func storedSubscriptions(t *testing.T, f *fixture) int {
	t.Helper()
	var n int
	if err := f.pg.PgRO.QueryRow(t.Context(),
		`select count(*) from billing_subscriptions where org_id = $1`, f.orgID).Scan(&n); err != nil {
		t.Fatalf("count subscriptions: %v", err)
	}
	return n
}

// The whole point: a buyer comes back and the plan is theirs with no delivery
// having arrived — the only path that works with no reachable webhook URL.
func TestConfirmCheckoutAppliesASettledCheckout(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	provider.checkout = subEvent(f.orgID, "sub00000000000000020", "prod_growth", corebilling.SubStatusActive)

	confirmed, err := f.svc.ConfirmCheckout(t.Context(), f.orgID, "cs_1", time.Now())
	if err != nil {
		t.Fatalf("ConfirmCheckout: %v", err)
	}
	if !confirmed {
		t.Fatal("confirmed = false, want true for a settled checkout")
	}

	ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, time.Now())
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if ent.Slug != "growth" {
		t.Errorf("slug = %q, want growth — the confirmed checkout did not reach the entitlement", ent.Slug)
	}
	if ent.SubStatus != corebilling.SubStatusActive {
		t.Errorf("sub status = %q, want active", ent.SubStatus)
	}
}

// The guard that makes a client-supplied session id safe. Without it, any admin
// could paste another org's session id and take its subscription.
func TestConfirmCheckoutRefusesASessionForAnotherOrg(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	provider.checkout = subEvent("some-other-org", "sub00000000000000021", "prod_growth", corebilling.SubStatusActive)

	confirmed, err := f.svc.ConfirmCheckout(t.Context(), f.orgID, "cs_1", time.Now())
	if !errors.Is(err, corebilling.ErrCheckoutNotForOrg) {
		t.Fatalf("err = %v, want ErrCheckoutNotForOrg", err)
	}
	if confirmed {
		t.Error("confirmed = true for another org's checkout")
	}
	if n := storedSubscriptions(t, f); n != 0 {
		t.Errorf("wrote %d subscription rows for another org's checkout, want 0", n)
	}
}

// The webhook may attribute by provider customer when metadata is missing;
// confirm must not, because the caller chose which id to hand over.
func TestConfirmCheckoutRefusesACheckoutCarryingNoOrg(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	event := subEvent(f.orgID, "sub00000000000000022", "prod_growth", corebilling.SubStatusActive)
	// Attribution by customer is what the webhook would fall back to, and it is
	// exactly what must not happen here.
	event.OrgID = ""
	provider.checkout = event

	if _, err := f.svc.ConfirmCheckout(t.Context(), f.orgID, "cs_1", time.Now()); !errors.Is(err, corebilling.ErrCheckoutNotForOrg) {
		t.Fatalf("err = %v, want ErrCheckoutNotForOrg", err)
	}
	if n := storedSubscriptions(t, f); n != 0 {
		t.Errorf("wrote %d subscription rows for an unattributed checkout, want 0", n)
	}
}

// The ordinary answer for a buyer who got back first. Not an error: the client
// keeps waiting for the webhook.
func TestConfirmCheckoutReportsAnUnsettledCheckout(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, _ := newPaidFixture(t)

	confirmed, err := f.svc.ConfirmCheckout(t.Context(), f.orgID, "cs_1", time.Now())
	if err != nil {
		t.Fatalf("ConfirmCheckout: %v", err)
	}
	if confirmed {
		t.Error("confirmed = true with no subscription at the provider")
	}
}

// A status pug has no word for is stored verbatim and is not live: the row is
// written (it leaves the org a customer to manage), the buyer told nothing.
func TestConfirmCheckoutDoesNotConfirmAPendingSubscription(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	provider.checkout = subEvent(f.orgID, "sub00000000000000023", "prod_growth", corebilling.SubStatus("pending"))

	confirmed, err := f.svc.ConfirmCheckout(t.Context(), f.orgID, "cs_1", time.Now())
	if err != nil {
		t.Fatalf("ConfirmCheckout: %v", err)
	}
	if confirmed {
		t.Error("confirmed = true for a pending subscription")
	}
	if n := storedSubscriptions(t, f); n != 1 {
		t.Errorf("stored %d subscription rows, want the pending one written", n)
	}
	ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, time.Now())
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if ent.Slug == "growth" {
		t.Error("a pending subscription granted the plan")
	}
}

// Somebody has paid and pug cannot render the amount. Loud, and returned rather
// than swallowed, because a person is waiting on the answer.
func TestConfirmCheckoutRefusesAForeignCurrency(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	event := subEvent(f.orgID, "sub00000000000000024", "prod_growth", corebilling.SubStatusActive)
	event.Currency = "EUR"
	provider.checkout = event

	if _, err := f.svc.ConfirmCheckout(t.Context(), f.orgID, "cs_1", time.Now()); !errors.Is(err, corebilling.ErrCurrencyNotSupported) {
		t.Fatalf("err = %v, want ErrCurrencyNotSupported", err)
	}
	if n := storedSubscriptions(t, f); n != 0 {
		t.Errorf("wrote %d rows for a currency pug cannot store, want 0", n)
	}
}

// A product no config key and no org row maps to. Same disposition: refuse, and
// say so, rather than write a subscription against nothing.
func TestConfirmCheckoutRefusesAnUnmappableProduct(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	provider.checkout = subEvent(f.orgID, "sub00000000000000025", "prod_unknown", corebilling.SubStatusActive)

	if _, err := f.svc.ConfirmCheckout(t.Context(), f.orgID, "cs_1", time.Now()); !errors.Is(err, corebilling.ErrNotPurchasable) {
		t.Fatalf("err = %v, want ErrNotPurchasable", err)
	}
	if n := storedSubscriptions(t, f); n != 0 {
		t.Errorf("wrote %d rows for an unmappable product, want 0", n)
	}
}

// The webhook beat the buyer home, so the CAS skips the confirm's write as stale.
// Reporting "not confirmed" would send someone who holds the plan into the poll.
func TestConfirmCheckoutConfirmsWhenTheWebhookLandedFirst(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	event := subEvent(f.orgID, "sub00000000000000026", "prod_growth", corebilling.SubStatusActive)
	provider.event = event
	provider.checkout = event

	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("wh_1", time.Now())); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}

	// An hour behind the delivery, so the CAS refuses the write outright.
	confirmed, err := f.svc.ConfirmCheckout(t.Context(), f.orgID, "cs_1", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("ConfirmCheckout: %v", err)
	}
	if !confirmed {
		t.Fatal("confirmed = false after the webhook already applied the same subscription")
	}
	if n := storedSubscriptions(t, f); n != 1 {
		t.Errorf("stored %d subscription rows, want 1 — the confirm duplicated the webhook's", n)
	}
}

// The buyer paid, the unique index refused the row, and the old plan is still in
// force. Reporting that as confirmed sends somebody now paying twice away happy.
func TestConfirmCheckoutRefusesASecondLiveSubscription(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	// A live subscription the org already holds — a cancellation that never
	// arrived, which is the deployment this whole path exists for.
	provider.event = subEvent(f.orgID, "sub00000000000000027", "prod_growth", corebilling.SubStatusActive)
	if err := f.svc.HandleDelivery(t.Context(), provider, delivery("wh_live", time.Now())); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}
	provider.checkout = subEvent(f.orgID, "sub00000000000000028", "prod_scale", corebilling.SubStatusActive)

	confirmed, err := f.svc.ConfirmCheckout(t.Context(), f.orgID, "cs_1", time.Now())
	if !errors.Is(err, corebilling.ErrTwoLiveSubscriptions) {
		t.Fatalf("err = %v, want ErrTwoLiveSubscriptions", err)
	}
	if confirmed {
		t.Error("confirmed = true for a subscription that was never written")
	}
	ent, err := f.svc.GetEntitlement(t.Context(), f.orgID, time.Now())
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if ent.Slug != "growth" {
		t.Errorf("slug = %q, want the untouched growth", ent.Slug)
	}
}

// A declined card. The provider says so, and saying "not settled yet" instead
// leaves the buyer polling for money that will never arrive.
func TestConfirmCheckoutSurfacesAFailedCheckout(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	provider.checkoutErr = corebilling.ErrCheckoutFailed

	confirmed, err := f.svc.ConfirmCheckout(t.Context(), f.orgID, "cs_1", time.Now())
	if !errors.Is(err, corebilling.ErrCheckoutFailed) {
		t.Fatalf("err = %v, want ErrCheckoutFailed", err)
	}
	if confirmed {
		t.Error("confirmed = true for a checkout that did not complete")
	}
	if n := storedSubscriptions(t, f); n != 0 {
		t.Errorf("wrote %d rows for a failed checkout, want 0", n)
	}
}

// A real subscription with no status is provider schema drift. It must not read
// as the ordinary "not settled yet", which would poll forever in silence.
func TestConfirmCheckoutRefusesASubscriptionWithNoStatus(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	event := subEvent(f.orgID, "sub00000000000000029", "prod_growth", corebilling.SubStatusActive)
	event.Status = ""
	provider.checkout = event

	confirmed, err := f.svc.ConfirmCheckout(t.Context(), f.orgID, "cs_1", time.Now())
	if err == nil {
		t.Fatal("err = nil for a subscription carrying no status, want an error")
	}
	if confirmed {
		t.Error("confirmed = true for a subscription carrying no status")
	}
	if n := storedSubscriptions(t, f); n != 0 {
		t.Errorf("wrote %d rows for a statusless subscription, want 0", n)
	}
}

// metadata.org_id is buyer-settable on a static payment link, so a checkout pug
// never opened must not confirm even when it names the caller's own org.
func TestConfirmCheckoutRefusesARefPugNeverMinted(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	event := subEvent(f.orgID, "sub00000000000000030", "prod_growth", corebilling.SubStatusActive)
	event.CheckoutRef = "ref_forged"
	provider.checkout = event

	if _, err := f.svc.ConfirmCheckout(t.Context(), f.orgID, "cs_1", time.Now()); !errors.Is(err, corebilling.ErrCheckoutNotForOrg) {
		t.Fatalf("err = %v, want ErrCheckoutNotForOrg", err)
	}
	if n := storedSubscriptions(t, f); n != 0 {
		t.Errorf("wrote %d subscription rows for a checkout pug never opened, want 0", n)
	}
}

// A payment link carries no ref and its metadata.org_id is the buyer's. Only the
// webhook places those, and only against a staged product.
func TestConfirmCheckoutRefusesACheckoutCarryingNoRef(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	event := subEvent(f.orgID, "sub00000000000000032", "prod_growth", corebilling.SubStatusActive)
	event.CheckoutRef = ""
	provider.checkout = event

	if _, err := f.svc.ConfirmCheckout(t.Context(), f.orgID, "cs_1", time.Now()); !errors.Is(err, corebilling.ErrCheckoutNotForOrg) {
		t.Fatalf("err = %v, want ErrCheckoutNotForOrg", err)
	}
	if n := storedSubscriptions(t, f); n != 0 {
		t.Errorf("wrote %d subscription rows for a checkout carrying no ref, want 0", n)
	}
}

// The ref outranks metadata: a session pug opened for another org stays that
// org's, whatever org_id the payload carries.
func TestConfirmCheckoutRefusesAnotherOrgsRef(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	other, err := dbwriteOrg(t, f.pg)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	seedCheckoutRef(t, f, other)

	event := subEvent(f.orgID, "sub00000000000000031", "prod_growth", corebilling.SubStatusActive)
	event.CheckoutRef = checkoutRef(other)
	provider.checkout = event

	if _, err := f.svc.ConfirmCheckout(t.Context(), f.orgID, "cs_1", time.Now()); !errors.Is(err, corebilling.ErrCheckoutNotForOrg) {
		t.Fatalf("err = %v, want ErrCheckoutNotForOrg", err)
	}
	if n := storedSubscriptions(t, f); n != 0 {
		t.Errorf("wrote %d subscription rows for another org's checkout, want 0", n)
	}
}

// The confirm path's half of the Clear race, and the one with a buyer blocked on
// the response: the product check runs on a record read outside the lock, so a
// clear can commit before applySubscription re-reads it. The re-read under the
// lock is what stops a live custom subscription being stored against a row that
// is gone -- which would resolve to the free floor for somebody who just paid.
func TestClearCannotStrandAConfirmInFlight(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	ctx := t.Context()

	productID := "prod_acme"
	if _, err := f.svc.SetPlan(ctx, f.orgID, actor, corebilling.Change{
		PlanSlug:          corebilling.SlugCustom,
		IncludedEvents:    new(int64(5_000_000)),
		ProviderProductID: &productID,
	}); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}

	tx, err := f.pg.PgW.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		`select pg_advisory_xact_lock(hashtext('billing_entitlement:' || $1::text))`, f.orgID); err != nil {
		t.Fatalf("take the entitlement lock: %v", err)
	}
	if _, err := tx.Exec(ctx, `delete from billing_entitlements where org_id = $1`, f.orgID); err != nil {
		t.Fatalf("delete the entitlement: %v", err)
	}

	provider.checkout = subEvent(f.orgID, "sub00000000000000029", productID, corebilling.SubStatusActive)
	var confirmed bool
	var confirmErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		confirmed, confirmErr = f.svc.ConfirmCheckout(ctx, f.orgID, "cs_1", time.Now())
	}()

	waitForEntitlementLockWaiter(t, f, done)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the clear: %v", err)
	}
	<-done

	if !errors.Is(confirmErr, corebilling.ErrNotPurchasable) {
		t.Errorf("err = %v, want ErrNotPurchasable", confirmErr)
	}
	if confirmed {
		t.Error("confirmed = true for a subscription that was never written")
	}
	if n := storedSubscriptions(t, f); n != 0 {
		t.Errorf("stored %d subscriptions, want 0 — a custom plan with no row behind it resolves free", n)
	}
}
