package billing

import "fmt"

// Line is one priced row of an invoice, in one of three shapes: a free or
// included band (blocks, no rate, no amount), a flat fee (an amount, no blocks),
// or a charged band (all four). Display only -- the total is Quote.TotalCents.
type Line struct {
	Description   string `json:"description"`
	Blocks        int64  `json:"blocks"`
	CentsPerBlock int64  `json:"cents_per_block"`
	AmountCents   int64  `json:"amount_cents"`
}

type Quote struct {
	Blocks     int64
	Lines      []Line
	TotalCents int64
}

// Blocks rounds events to the nearest block; a tie rounds up.
func Blocks(events, blockEvents int64) int64 {
	if events <= 0 || blockEvents <= 0 {
		return 0
	}
	return (events + blockEvents/2) / blockEvents
}

// Price is the one pricing function: the estimate, the invoice, the CLI preview
// and the tests all go through it.
func Price(card RateCard, events int64) Quote {
	blocks := Blocks(events, card.BlockEvents)
	q := Quote{Blocks: blocks}
	from := int64(0)
	if free := min(card.FreeBlocks, blocks); free > 0 {
		q.Lines = append(q.Lines, Line{Description: blockRange(1, free) + " free", Blocks: free})
		from = free
	}
	for _, t := range card.Tiers {
		if from >= blocks {
			break
		}
		to := t.UpToBlock
		if to == 0 || to > blocks {
			to = blocks
		}
		if to <= from {
			continue
		}
		n := to - from
		q.Lines = append(q.Lines, Line{
			Description:   blockRange(from+1, to),
			Blocks:        n,
			CentsPerBlock: t.CentsPerBlock,
			AmountCents:   n * t.CentsPerBlock,
		})
		q.TotalCents += n * t.CentsPerBlock
		from = to
	}
	return q
}

// PriceCustom prices a negotiated deal: the fee every period, and the block rate
// over the allowance.
func PriceCustom(t CustomTerms, events int64) Quote {
	blocks := Blocks(events, BlockEvents)
	q := Quote{Blocks: blocks}
	if t.FlatFeeCents > 0 {
		q.Lines = append(q.Lines, Line{Description: "flat fee", AmountCents: t.FlatFeeCents})
		q.TotalCents += t.FlatFeeCents
	}
	if t.BlockRateCents <= 0 {
		return q
	}
	allowance := t.IncludedEvents / BlockEvents
	if allowance > 0 && blocks > 0 {
		q.Lines = append(q.Lines, Line{Description: blockRange(1, min(allowance, blocks)) + " included", Blocks: min(allowance, blocks)})
	}
	if over := blocks - allowance; over > 0 {
		q.Lines = append(q.Lines, Line{
			Description:   blockRange(allowance+1, blocks),
			Blocks:        over,
			CentsPerBlock: t.BlockRateCents,
			AmountCents:   over * t.BlockRateCents,
		})
		q.TotalCents += over * t.BlockRateCents
	}
	return q
}

func blockRange(from, to int64) string {
	if from == to {
		return fmt.Sprintf("block %d", from)
	}
	return fmt.Sprintf("blocks %d-%d", from, to)
}

// Pricing is what an invoice was priced on, snapshotted so a later catalog edit
// cannot change what it says it charged.
type Pricing struct {
	Card  *RateCard    `json:"card,omitempty"`
	Terms *CustomTerms `json:"terms,omitempty"`
}

// Quote prices a period. The bool is false when nothing here can price one, so
// a caller cannot mistake a refusal for a zero-cent period.
func (p Pricing) Quote(events int64) (Quote, bool) {
	if p.IsZero() {
		return Quote{}, false
	}
	if p.Terms != nil {
		return PriceCustom(*p.Terms, events), true
	}
	return Price(*p.Card, events), true
}

// IsZero is "nothing here can price a period": no terms worth money, and no card
// this build can divide by.
func (p Pricing) IsZero() bool {
	switch {
	case p.Terms != nil:
		return p.Terms.FlatFeeCents <= 0 && p.Terms.BlockRateCents <= 0
	case p.Card != nil:
		return p.Card.BlockEvents <= 0 || len(p.Card.Tiers) == 0
	}
	return true
}
