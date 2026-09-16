package billing

import (
	"slices"
	"testing"
)

func TestNewServiceRefusesAMalformedRateCard(t *testing.T) {
	original := rateCards
	t.Cleanup(func() { rateCards = original })

	card := func(mutate func(c *RateCard)) []RateCard {
		c := copyCard(original[0])
		mutate(&c)
		return []RateCard{c}
	}
	newer := copyCard(original[0])
	newer.Slug = "usage-2099-01-1"
	for name, cards := range map[string][]RateCard{
		"no card":                     nil,
		"a slug declared twice":       {copyCard(original[0]), copyCard(original[0])},
		"every card retired":          card(func(c *RateCard) { c.Retired = true }),
		"two current cards":           {copyCard(original[0]), newer},
		"the free slug":               card(func(c *RateCard) { c.Slug = SlugFree }),
		"an empty slug":               card(func(c *RateCard) { c.Slug = "" }),
		"no retention":                card(func(c *RateCard) { c.RetentionDays = 0 }),
		"the custom slug":             card(func(c *RateCard) { c.Slug = SlugCustom }),
		"a foreign currency":          card(func(c *RateCard) { c.Currency = "EUR" }),
		"negative free events":        card(func(c *RateCard) { c.FreeEvents = -1 }),
		"no tiers":                    card(func(c *RateCard) { c.Tiers = nil }),
		"an unbounded tier not last":  card(func(c *RateCard) { c.Tiers[0].UpToEvents = 0 }),
		"a bounded last tier":         card(func(c *RateCard) { c.Tiers[len(c.Tiers)-1].UpToEvents = 500_000_000 }),
		"tiers not ascending":         card(func(c *RateCard) { c.Tiers[1].UpToEvents = c.Tiers[0].UpToEvents }),
		"a tier inside the free band": card(func(c *RateCard) { c.FreeEvents = c.Tiers[0].UpToEvents }),
		"a negative rate":             card(func(c *RateCard) { c.Tiers[0].CentsPerMillion = -1 }),
	} {
		t.Run(name, func(t *testing.T) {
			rateCards = cards
			if _, err := NewService(nil, nil, true, nil); err == nil {
				t.Errorf("NewService accepted a rate card catalog with %s", name)
			}
		})
	}

	rateCards = original
	if _, err := NewService(nil, nil, true, nil); err != nil {
		t.Fatalf("NewService refused the real catalog: %v", err)
	}
}

func TestCurrentCardSkipsARetiredCard(t *testing.T) {
	original := rateCards
	t.Cleanup(func() { rateCards = original })

	want := CurrentCard().Slug
	newer := CurrentCard()
	newer.Slug, newer.Retired = "usage-2099-01-1", true
	rateCards = append(slices.Clone(original), newer)
	if got := CurrentCard().Slug; got != want {
		t.Errorf("CurrentCard = %q, want %q: a retired newer card was handed out as current", got, want)
	}

	older := copyCard(original[0])
	older.Retired = true
	newer.Retired = false
	rateCards = []RateCard{older, newer}
	if got := CurrentCard().Slug; got != newer.Slug {
		t.Errorf("CurrentCard = %q, want %q: a retired older card was handed out as current", got, newer.Slug)
	}
}
