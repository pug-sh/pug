package billing

import (
	"testing"
	"time"
)

// Which card prices a close's days, and which mandates bill none.
func TestBillableSegments(t *testing.T) {
	original := rateCards
	t.Cleanup(func() { rateCards = original })
	older := copyCard(original[0])
	older.Slug, older.Retired = "usage-2025-01-1", true
	rateCards = append([]RateCard{older}, original...)
	current := CurrentCard()

	day := func(d int) time.Time { return time.Date(2026, time.August, d, 0, 0, 0, 0, time.UTC) }
	created := time.Date(2025, time.March, 10, 0, 0, 0, 0, time.UTC)
	p := period{start: day(10), end: day(10).AddDate(0, 1, 0)}
	mandate := func(slug string, from, until time.Time) Subscription {
		sub := Subscription{OnDemand: true, PlanSlug: slug, Status: SubStatusActive, CreateTime: from}
		if !until.IsZero() {
			sub.Status, sub.EndedAt = SubStatusCancelled, until
		}
		return sub
	}
	segments := func(rec Record, subs ...Subscription) []segment {
		return billableSegments(created, rec, subs, p, time.Time{})
	}
	slugs := func(segs []segment) []string {
		var out []string
		for _, s := range segs {
			out = append(out, s.ent.Slug)
		}
		return out
	}

	t.Run("priced on the latest mandate", func(t *testing.T) {
		segs := segments(Record{}, mandate(current.Slug, day(20), time.Time{}), mandate(older.Slug, day(10), day(15)))
		if len(segs) != 1 || segs[0].ent.Slug != current.Slug {
			t.Errorf("segments priced on %v, want one on %q", slugs(segs), current.Slug)
		}
	})

	t.Run("a cancelled mandate keeps its card", func(t *testing.T) {
		segs := segments(Record{}, mandate(older.Slug, day(10), day(15)))
		if len(segs) != 1 || segs[0].ent.Slug != older.Slug {
			t.Errorf("segments priced on %v, want one on %q", slugs(segs), older.Slug)
		}
	})

	t.Run("card days beside a deal keep the card's allowance", func(t *testing.T) {
		deal := Record{
			Present: true, PlanSlug: SlugCustom, RateCentsPerMillion: 3_000,
			IncludedEventsOverride: 5_000_000, TermsEffectiveAt: day(30),
		}
		segs := segments(deal, mandate(current.Slug, day(10), time.Time{}))
		if len(segs) != 2 || segs[0].ent.Card == nil || segs[0].ent.Card.FreeEvents != current.FreeEvents {
			t.Errorf("segments priced on %v, want the card days first with %d free", slugs(segs), current.FreeEvents)
		}
	})

	t.Run("mandates that bill nothing", func(t *testing.T) {
		recurring := mandate(current.Slug, day(10), time.Time{})
		recurring.OnDemand = false
		undated := mandate(current.Slug, day(12), time.Time{})
		undated.Status = SubStatusCancelled
		pending := mandate(current.Slug, day(12), time.Time{})
		pending.Status = "pending"
		for name, sub := range map[string]Subscription{
			"recurring": recurring, "an undated end": undated, "an unknown status": pending,
		} {
			if segs := segments(Record{}, sub); len(segs) != 0 {
				t.Errorf("%s: billed %d segments, want none", name, len(segs))
			}
		}
	})
}
