package subscription_test

import (
	"testing"
	"time"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
	"github.com/pug-sh/pug/internal/testutil"
)

// graceDeadline reads the column straight off the row, so these tests pin the
// write rule rather than what a read path makes of it.
func graceDeadline(t *testing.T, f *fixture, subID string) *time.Time {
	t.Helper()
	var at *time.Time
	if err := f.pg.PgRO.QueryRow(t.Context(),
		`select grace_period_ends_at from billing_subscriptions where provider = $1 and provider_sub_id = $2`,
		fakeProviderName, subID).Scan(&at); err != nil {
		t.Fatalf("read grace_period_ends_at: %v", err)
	}
	return at
}

// pastDue is a delivery inside the provider's grace period.
func pastDue(orgID, subID string, deadline time.Time) corebilling.SubscriptionEvent {
	event := subEvent(orgID, subID, "prod_u", corebilling.SubStatusPastDue)
	event.GracePeriodEndsAt, event.GracePeriodEndsAtKnown = deadline, true
	return event
}

// deliverPastDue delivers a grace window stamped a minute ago, so whatever a test
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
	if ent.SubStatus != corebilling.SubStatusPastDue || !ent.SubGracePeriodEndsAt.Equal(deadline) {
		t.Errorf("entitlement = (%s, %s), want (past_due, %s)", ent.SubStatus, ent.SubGracePeriodEndsAt, deadline)
	}
}

// A subscription is on file long before its card fails, so the deadline lands on an
// existing row, through the update half of the write; a later delivery's replaces it.
func TestAnActiveSubscriptionGoingPastDueStoresItsDeadline(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	active := subEvent(f.orgID, "sub_grace", "prod_u", corebilling.SubStatusActive)
	active.GracePeriodEndsAtKnown = true
	first := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Second)
	second := first.Add(24 * time.Hour)
	for i, step := range []struct {
		webhookID string
		event     corebilling.SubscriptionEvent
		want      *time.Time
	}{
		{"wh_active", active, nil},
		{"wh_first", pastDue(f.orgID, "sub_grace", first), &first},
		{"wh_second", pastDue(f.orgID, "sub_grace", second), &second},
	} {
		provider.event = step.event
		// Each a minute newer than the last, so each lands.
		if err := f.svc.HandleDelivery(t.Context(),
			delivery(step.webhookID, time.Now().Add(time.Duration(i-3)*time.Minute))); err != nil {
			t.Fatalf("HandleDelivery %s: %v", step.webhookID, err)
		}
		got := graceDeadline(t, f, "sub_grace")
		if (got == nil) != (step.want == nil) || (got != nil && !got.Equal(*step.want)) {
			t.Fatalf("after %s grace_period_ends_at = %v, want %v", step.webhookID, got, step.want)
		}
	}
}

// An event that cannot see the deadline says nothing about it, on first sight as on
// every update: a date beside an unknown is never stored.
func TestAnUnseenDeadlineIsNeverStored(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f, provider := newPaidFixture(t)
	provider.event = pastDue(f.orgID, "sub_grace", time.Now().Add(72*time.Hour))
	provider.event.GracePeriodEndsAtKnown = false
	if err := f.svc.HandleDelivery(t.Context(), delivery("wh_unseen", time.Now())); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}
	if got := graceDeadline(t, f, "sub_grace"); got != nil {
		t.Errorf("grace_period_ends_at = %s from an event that could not see it, want none", got)
	}
}

// Only a delivery carries the deadline, and reconcile re-reads every subscription on
// every pass with a newer stamp: were the column replaced like the rest of the row,
// the first pass after the delivery would erase it while the window is still open.
func TestAReadKeepsTheGraceDeadlineWhileTheWindowIsOpen(t *testing.T) {
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
		t.Errorf("grace_period_ends_at = %v after a read, want %s kept", got, deadline)
	}
}

// Outside the provider's grace window there is no deadline. A delivery says so, and
// so does a read in any state but the window's own — a hold included, where pug's
// status stays past_due. A read that can tell nothing still clears it once the
// subscription stops showing past_due.
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
			event.GracePeriodEndsAtKnown = true
			return event
		}},
		{name: "a delivery after the window ended in a hold", next: func(orgID string) corebilling.SubscriptionEvent {
			event := subEvent(orgID, "sub_grace", "prod_u", corebilling.SubStatusPastDue)
			event.ProviderStatus, event.GracePeriodEndsAtKnown = "on_hold", true
			return event
		}},
		{name: "a delivery after the window ended in a cancellation", next: func(orgID string) corebilling.SubscriptionEvent {
			event := subEvent(orgID, "sub_grace", "prod_u", corebilling.SubStatusCancelled)
			event.GracePeriodEndsAtKnown = true
			return event
		}},
		{name: "a read after the card recovered", read: true, next: func(orgID string) corebilling.SubscriptionEvent {
			return subEvent(orgID, "sub_grace", "prod_u", corebilling.SubStatusActive)
		}},
		{name: "a read after the window ended in a hold", read: true, next: func(orgID string) corebilling.SubscriptionEvent {
			event := subEvent(orgID, "sub_grace", "prod_u", corebilling.SubStatusPastDue)
			event.ProviderStatus, event.GracePeriodEndsAtKnown = "on_hold", true
			return event
		}},
		{name: "a read after a cancellation", read: true, next: func(orgID string) corebilling.SubscriptionEvent {
			return subEvent(orgID, "sub_grace", "prod_u", corebilling.SubStatusCancelled)
		}},
		{name: "a read after it expired", read: true, next: func(orgID string) corebilling.SubscriptionEvent {
			return subEvent(orgID, "sub_grace", "prod_u", corebilling.SubStatusExpired)
		}},
		{name: "a read while paused", read: true, next: func(orgID string) corebilling.SubscriptionEvent {
			return subEvent(orgID, "sub_grace", "prod_u", corebilling.SubStatusPaused)
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
				t.Errorf("grace_period_ends_at = %s, want none", got)
			}
		})
	}
}

// 023 only adds a column, and its down has to take it away again.
func TestMigration023RoundTrips(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	hasColumn := func() bool {
		t.Helper()
		var n int
		if err := pg.PgW.QueryRow(t.Context(),
			`select count(*) from information_schema.columns
			 where table_name = 'billing_subscriptions' and column_name = 'grace_period_ends_at'`).Scan(&n); err != nil {
			t.Fatalf("read the schema: %v", err)
		}
		return n == 1
	}
	if !hasColumn() {
		t.Fatal("023 did not add grace_period_ends_at")
	}
	migrations := testutil.PostgresMigrations(t, pg)
	if _, err := migrations.DownTo(t.Context(), 22); err != nil {
		t.Fatalf("migrate down to 022: %v", err)
	}
	if hasColumn() {
		t.Fatal("023's down left grace_period_ends_at behind")
	}
	if _, err := migrations.UpTo(t.Context(), 23); err != nil {
		t.Fatalf("migrate up to 023: %v", err)
	}
	if !hasColumn() {
		t.Fatal("023 did not add grace_period_ends_at back")
	}
}
