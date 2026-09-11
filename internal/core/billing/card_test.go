package billing

import "testing"

// A malformed card must fail wiring, not an invoice: every shape checkCatalog
// refuses, each swapped in for the test's duration.
func TestCheckCatalogRefusesAMalformedCard(t *testing.T) {
	original := catalog
	t.Cleanup(func() { catalog = original })

	good := copyCard(original[0])
	cases := map[string]func(c *RateCard){
		"no tiers":                   func(c *RateCard) { c.Tiers = nil },
		"unbounded tier not last":    func(c *RateCard) { c.Tiers[0].UpToBlock = 0 },
		"last tier bounded":          func(c *RateCard) { c.Tiers[len(c.Tiers)-1].UpToBlock = 99 },
		"tiers not ascending":        func(c *RateCard) { c.Tiers[1].UpToBlock = 5 },
		"tier below the free blocks": func(c *RateCard) { c.FreeBlocks = 10 },
		"negative rate":              func(c *RateCard) { c.Tiers[0].CentsPerBlock = -1 },
		"zero block size":            func(c *RateCard) { c.BlockEvents = 0 },
		"foreign currency":           func(c *RateCard) { c.Currency = "EUR" },
		"floor slug":                 func(c *RateCard) { c.Slug = SlugFree },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			bad := copyCard(good)
			mutate(&bad)
			catalog = []RateCard{bad}
			if err := checkCatalog(); err == nil {
				t.Errorf("checkCatalog accepted a card with %s", name)
			}
		})
	}
	t.Run("every card retired", func(t *testing.T) {
		retired := copyCard(good)
		retired.Retired = true
		catalog = []RateCard{retired}
		if err := checkCatalog(); err == nil {
			t.Error("checkCatalog accepted a catalog with nothing current")
		}
	})
	t.Run("duplicate slug", func(t *testing.T) {
		catalog = []RateCard{copyCard(good), copyCard(good)}
		if err := checkCatalog(); err == nil {
			t.Error("checkCatalog accepted a duplicated slug")
		}
	})
	catalog = original
	if err := checkCatalog(); err != nil {
		t.Fatalf("the real catalog does not pass its own check: %v", err)
	}
}

// CurrentCard is the newest card that is not retired, whatever comes after it.
func TestCurrentCardSkipsRetiredCards(t *testing.T) {
	original := catalog
	t.Cleanup(func() { catalog = original })

	next := copyCard(original[0])
	next.Slug = "usage-2027-01"
	next.Retired = true
	catalog = append(append([]RateCard(nil), original...), next)
	if got := CurrentCard().Slug; got != CurrentSlug {
		t.Errorf("CurrentCard = %q, want %q — a retired card was handed to new orgs", got, CurrentSlug)
	}
}
