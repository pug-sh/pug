package billing

import (
	"testing"
	"time"
)

// Repricing mints a new card and retires the old one. A holder keeps resolving
// against the card they agreed to; an org with no mandate sees the current one.
func TestRetiredCardKeepsResolvingForItsHolder(t *testing.T) {
	original := catalog
	t.Cleanup(func() { catalog = original })

	next := copyCard(original[0])
	next.Slug = "usage-2027-01"
	next.FreeBlocks = 0
	old := copyCard(original[0])
	old.Retired = true
	catalog = []RateCard{old, next}

	created := time.Date(2025, 1, 10, 0, 0, 0, 0, time.UTC)
	now := time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC)
	holder := &Subscription{PlanSlug: old.Slug, Status: SubStatusActive, OnDemand: true}

	pinned := Resolve(created, Record{}, holder, now, true)
	if pinned.Slug != old.Slug || pinned.Card == nil || pinned.Card.FreeBlocks != 1 {
		t.Errorf("holder resolved %q (free blocks %v), want the retired card with its free block", pinned.Slug, pinned.Card)
	}
	fresh := Resolve(created, Record{}, nil, now, true)
	if fresh.Slug != next.Slug || fresh.Card == nil || fresh.Card.FreeBlocks != 0 {
		t.Errorf("an org with no mandate resolved %q, want the current card", fresh.Slug)
	}
	if CurrentCard().Slug != next.Slug {
		t.Errorf("CurrentCard = %q, want %q", CurrentCard().Slug, next.Slug)
	}
}

// The read half above keeps a holder on their card. This is the write half: an
// operator must not be able to put a new org on a card that has been withdrawn.
func TestRetiredCardCannotBeGrantedToANewOrg(t *testing.T) {
	retired := RateCard{Slug: "usage-2026-09", Retired: true}
	current := RateCard{Slug: "usage-2027-01"}

	if !retiredGrant(Record{}, retired) {
		t.Error("an org with no plan was offered a retired card")
	}
	if !retiredGrant(Record{PlanSlug: "usage-2027-01"}, retired) {
		t.Error("an org on the current card was moved onto a retired one")
	}
	if retiredGrant(Record{PlanSlug: "usage-2026-09"}, retired) {
		t.Error("an org already holding the retired card was refused its own renewal")
	}
	if retiredGrant(Record{}, current) {
		t.Error("the current card was refused")
	}
}
