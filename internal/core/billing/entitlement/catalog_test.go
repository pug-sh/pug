package entitlement

import "testing"

func TestCatalogIsWellFormed(t *testing.T) {
	// checkRateCards is what NewService calls at wiring time; a malformed catalog
	// must fail startup rather than mis-price a request.
	if err := checkRateCards(); err != nil {
		t.Fatalf("checkRateCards: %v", err)
	}
}

func TestCurrentCardIsTheOnlyLiveOne(t *testing.T) {
	live := 0
	for _, c := range Cards() {
		if !c.Retired {
			live++
		}
	}
	if live != 1 {
		t.Fatalf("live cards = %d, want exactly one", live)
	}
	if CurrentCard().Retired {
		t.Error("CurrentCard returned a retired card")
	}
}

func TestCardsReturnsCopies(t *testing.T) {
	// A caller must not be able to reprice the catalog by mutating what it got back.
	got := Cards()
	if len(got) == 0 || len(got[0].Tiers) == 0 {
		t.Fatal("catalog is empty")
	}
	want := got[0].Tiers[0].CentsPerMillion
	got[0].Tiers[0].CentsPerMillion = 999_999
	if again := Cards()[0].Tiers[0].CentsPerMillion; again != want {
		t.Errorf("mutating a returned card changed the catalog: %d", again)
	}
}

func TestCardBySlugReportsAnUnknownSlug(t *testing.T) {
	if _, ok := CardBySlug("no-such-card"); ok {
		t.Error("CardBySlug accepted a slug the catalog does not have")
	}
	if _, ok := CardBySlug(CurrentCard().Slug); !ok {
		t.Error("CardBySlug rejected the current card")
	}
}

func TestIsCardSlugRejectsStates(t *testing.T) {
	for _, s := range []string{"", SlugFree, SlugTrial, SlugCustom} {
		if isCardSlug(s) {
			t.Errorf("isCardSlug(%q) = true; free, trial and custom are states, not cards", s)
		}
	}
	if !isCardSlug("usage-2026-09-1") {
		t.Error("isCardSlug rejected a real card slug")
	}
}
