package subscription_test

import (
	"testing"
	"time"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
)

// graceDeadline reads the column straight off the row, so these tests pin the
// write rule rather than what a read path makes of it.
func graceDeadline(t *testing.T, f *fixture, subID string) *time.Time {
	t.Helper()
	var at *time.Time
	if err := f.pg.PgRO.QueryRow(t.Context(),
		`select past_due_ends_at from billing_subscriptions where provider = $1 and provider_sub_id = $2`,
		fakeProviderName, subID).Scan(&at); err != nil {
		t.Fatalf("read past_due_ends_at: %v", err)
	}
	return at
}

// pastDue is a delivery inside the provider's grace period.
func pastDue(orgID, subID string, deadline time.Time) corebilling.SubscriptionEvent {
	event := subEvent(orgID, subID, "prod_u", corebilling.SubStatusPastDue)
	event.PastDueEndsAt, event.PastDueEndsAtKnown = deadline, true
	return event
}

// deliverPastDue stores a grace window a minute in the past, so whatever a test
// applies next is newer and lands.
func deliverPastDue(t *testing.T, f *fixture, provider *fakeProvider, deadline time.Time) {
	t.Helper()
	provider.event = pastDue(f.orgID, "sub_grace", deadline)
	if err := f.svc.HandleDelivery(t.Context(), delivery("wh_grace", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}
}

// The deadline reaches the entitlement the banner is rendered from.
func TestADeliveryStoresTheGraceDeadline(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	deadline := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Second)
	deliverPastDue(t, f, provider, deadline)

	ent, err := f.entitlements.GetEntitlement(t.Context(), f.orgID, time.Now())
	if err != nil {
		t.Fatalf("GetEntitlement: %v", err)
	}
	if ent.SubStatus != corebilling.SubStatusPastDue || !ent.SubPastDueEndsAt.Equal(deadline) {
		t.Errorf("entitlement = (%s, %s), want (past_due, %s)", ent.SubStatus, ent.SubPastDueEndsAt, deadline)
	}
}

// Only a delivery carries the deadline, and reconcile re-reads every live
// subscription on every pass with a newer stamp: were the column replaced like the
// rest of the row, the first pass after the delivery would erase it.
func TestAReadKeepsTheGraceDeadlineWhileTheCardFails(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	deadline := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Second)
	deliverPastDue(t, f, provider, deadline)

	read := subEvent(f.orgID, "sub_grace", "prod_u", corebilling.SubStatusPastDue)
	svc := f.svcWithProvider(t, &fetchProvider{
		fakeProvider: *provider,
		remote:       map[string]corebilling.SubscriptionEvent{"sub_grace": read},
	})
	report, err := svc.Reconcile(t.Context(), time.Now())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if report.Applied != 1 {
		t.Fatalf("applied = %d, want 1 — the read has to land for this to test anything", report.Applied)
	}
	if got := graceDeadline(t, f, "sub_grace"); got == nil || !got.Equal(deadline) {
		t.Errorf("past_due_ends_at = %v after a read, want %s kept", got, deadline)
	}
}

// Anything but a failing card has no deadline. A delivery says so outright; a read
// cannot see one, but a read that no longer shows the card failing cannot be
// keeping one either.
func TestTheGraceDeadlineClearsOnceTheWindowCloses(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	cases := []struct {
		name string
		next func(orgID string) corebilling.SubscriptionEvent
		read bool
	}{
		{name: "a delivery after the card recovered", next: func(orgID string) corebilling.SubscriptionEvent {
			event := subEvent(orgID, "sub_grace", "prod_u", corebilling.SubStatusActive)
			event.PastDueEndsAtKnown = true
			return event
		}},
		{name: "a delivery after the window ended in a hold", next: func(orgID string) corebilling.SubscriptionEvent {
			event := subEvent(orgID, "sub_grace", "prod_u", corebilling.SubStatusPastDue)
			event.ProviderStatus, event.PastDueEndsAtKnown = "on_hold", true
			return event
		}},
		{name: "a delivery after the window ended in a cancellation", next: func(orgID string) corebilling.SubscriptionEvent {
			event := subEvent(orgID, "sub_grace", "prod_u", corebilling.SubStatusCancelled)
			event.PastDueEndsAtKnown = true
			return event
		}},
		{name: "a read after the card recovered", read: true, next: func(orgID string) corebilling.SubscriptionEvent {
			return subEvent(orgID, "sub_grace", "prod_u", corebilling.SubStatusActive)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, provider := newPaidFixture(t)
			deliverPastDue(t, f, provider, time.Now().Add(72*time.Hour))
			if graceDeadline(t, f, "sub_grace") == nil {
				t.Fatal("the deadline was never stored")
			}

			next := tc.next(f.orgID)
			if tc.read {
				svc := f.svcWithProvider(t, &fetchProvider{
					fakeProvider: *provider,
					remote:       map[string]corebilling.SubscriptionEvent{"sub_grace": next},
				})
				if _, err := svc.Reconcile(t.Context(), time.Now()); err != nil {
					t.Fatalf("Reconcile: %v", err)
				}
			} else {
				provider.event = next
				if err := f.svc.HandleDelivery(t.Context(), delivery("wh_next", time.Now())); err != nil {
					t.Fatalf("HandleDelivery: %v", err)
				}
			}
			if got := graceDeadline(t, f, "sub_grace"); got != nil {
				t.Errorf("past_due_ends_at = %s, want none", got)
			}
		})
	}
}
