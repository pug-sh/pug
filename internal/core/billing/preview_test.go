package billing_test

import (
	"errors"
	"testing"
	"time"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
)

// Preview's job is the dispatch: a deal is priced on its own terms and a card pin
// on the card. The arithmetic itself is price_test.go's.
func TestPreviewPricesADealOnItsOwnTerms(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, corebilling.Change{
		PlanSlug:            corebilling.SlugCustom,
		FlatFeeCents:        new(int64(40_000)),
		RateCentsPerMillion: new(int64(3_000)),
		IncludedEvents:      new(int64(5_000_000)),
	}); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}

	ent, quote, err := f.svc.Preview(t.Context(), f.orgID, 7_000_000, time.Now())
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if ent.Terms == nil || ent.Card != nil {
		t.Fatalf("terms = %v, card = %v, want the deal and no card", ent.Terms, ent.Card)
	}
	// $400.00 flat, plus 2M events over the allowance at $30.00 per million.
	if quote.TotalCents != 46_000 {
		t.Errorf("total = %d, want 46000", quote.TotalCents)
	}
}

func TestPreviewPricesACardPinOnTheCard(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	if _, err := f.svc.SetPlan(t.Context(), f.orgID, actor, pinCard()); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}

	ent, quote, err := f.svc.Preview(t.Context(), f.orgID, 1_000_000, time.Now())
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if ent.Card == nil || ent.Terms != nil {
		t.Fatalf("card = %v, terms = %v, want the card and no deal", ent.Card, ent.Terms)
	}
	if want := corebilling.Price(currentCard(), 1_000_000).TotalCents; quote.TotalCents != want {
		t.Errorf("total = %d, want %d — the card's own price", quote.TotalCents, want)
	}
}

// A slug no card answers to prices nothing, so it must refuse rather than quote
// the current card's rates at a customer nobody sold them to.
func TestPreviewRefusesASlugItCannotPrice(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newFixture(t)
	if _, err := f.pg.PgW.Exec(t.Context(),
		`insert into billing_entitlements (org_id, plan_slug) values ($1, 'growth')`, f.orgID); err != nil {
		t.Fatalf("seed a dropped card: %v", err)
	}

	if _, _, err := f.svc.Preview(t.Context(), f.orgID, 1_000_000, time.Now()); !errors.Is(err, corebilling.ErrPlanNotFound) {
		t.Errorf("err = %v, want ErrPlanNotFound", err)
	}
}
