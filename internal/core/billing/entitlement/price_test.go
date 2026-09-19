package entitlement

import "testing"

func TestPriceGraduatesAcrossTiers(t *testing.T) {
	card := RateCard{
		FreeEvents: 100_000,
		Tiers: []Tier{
			{UpToEvents: 2_000_000, CentsPerMillion: 4_000},
			{UpToEvents: 15_000_000, CentsPerMillion: 2_800},
			{UpToEvents: 0, CentsPerMillion: 700},
		},
	}
	cases := []struct {
		name   string
		events int64
		want   int64
	}{
		{"nothing sent", 0, 0},
		{"inside the free allowance", 50_000, 0},
		{"exactly the free allowance", 100_000, 0},
		// 1.9M at 4000/M = 7600
		{"first tier only", 2_000_000, 7_600},
		// the above, then 13M at 2800/M = 36400
		{"through the second tier", 15_000_000, 44_000},
		// the above, then 5M at 700/M = 3500 on the unbounded tier
		{"into the unbounded tier", 20_000_000, 47_500},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := Price(card, tc.events)
			if q.TotalCents != tc.want {
				t.Errorf("TotalCents = %d, want %d", q.TotalCents, tc.want)
			}
			var sum int64
			for _, l := range q.Lines {
				sum += l.AmountCents
			}
			if sum != q.TotalCents {
				t.Errorf("lines sum to %d but TotalCents is %d; a customer's bill must add up", sum, q.TotalCents)
			}
			if q.Lines == nil {
				t.Error("Lines is nil; it is serialized as an array and must never be null")
			}
		})
	}
}

func TestPriceCustomFlatFeeOnlyEmitsOneLine(t *testing.T) {
	// This shape is what makes a fixed-price plan expressible as a deal: a flat fee,
	// an allowance, and no overage however much is sent.
	q := PriceCustom(CustomTerms{FlatFeeCents: 2_000, IncludedEvents: 500_000}, 750_000)
	if q.TotalCents != 2_000 {
		t.Errorf("TotalCents = %d, want 2000: no rate means no overage", q.TotalCents)
	}
	if len(q.Lines) != 1 {
		t.Fatalf("Lines = %d, want exactly the flat fee", len(q.Lines))
	}
	if q.Lines[0].Description != "flat fee" {
		t.Errorf("Lines[0].Description = %q, want \"flat fee\"", q.Lines[0].Description)
	}
}

func TestPriceCustomChargesOverTheAllowance(t *testing.T) {
	// 1M over a 500k allowance at 3000/M = 3000, plus the flat fee.
	q := PriceCustom(CustomTerms{FlatFeeCents: 4_000, RateCentsPerMillion: 3_000, IncludedEvents: 500_000}, 1_500_000)
	if want := int64(4_000 + 3_000); q.TotalCents != want {
		t.Errorf("TotalCents = %d, want %d", q.TotalCents, want)
	}
	var sum int64
	for _, l := range q.Lines {
		sum += l.AmountCents
	}
	if sum != q.TotalCents {
		t.Errorf("lines sum to %d but TotalCents is %d", sum, q.TotalCents)
	}
}
