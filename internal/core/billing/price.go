package billing

import (
	"strconv"
	"strings"
)

type Line struct {
	Description     string `json:"description"`
	Events          int64  `json:"events"`
	CentsPerMillion int64  `json:"cents_per_million"`
	AmountCents     int64  `json:"amount_cents"`
}

type Quote struct {
	Events     int64
	Lines      []Line
	TotalCents int64
}

// Price and PriceCustom are the only pricing paths; the card or terms come from the resolved entitlement.
func Price(card RateCard, events int64) Quote {
	q := Quote{Events: events, Lines: []Line{}} // never nil: the invoice stores lines as a jsonb array
	from := int64(0)
	if free := min(card.FreeEvents, events); free > 0 {
		q.add(Line{Description: eventRange(1, free) + " free", Events: free})
		from = free
	}
	for _, t := range card.Tiers {
		to := events
		if t.UpToEvents > 0 {
			to = min(t.UpToEvents, events)
		}
		if to > from {
			q.add(band(from, to, t.CentsPerMillion))
			from = to
		}
	}
	return q
}

// quote picks the pricing function from the resolved entitlement, the one place
// that choice is made. False is a slug no card answers to.
func (e Entitlement) quote(events int64) (Quote, bool) {
	switch {
	case e.Terms != nil:
		return PriceCustom(*e.Terms, events), true
	case e.Card != nil:
		return Price(*e.Card, events), true
	}
	return Quote{}, false
}

func PriceCustom(t CustomTerms, events int64) Quote {
	q := Quote{Events: events, Lines: []Line{}}
	if t.FlatFeeCents > 0 {
		q.add(Line{Description: "flat fee", AmountCents: t.FlatFeeCents})
	}
	if t.RateCentsPerMillion <= 0 {
		return q
	}
	if included := min(t.IncludedEvents, events); included > 0 {
		q.add(Line{Description: eventRange(1, included) + " included", Events: included})
	}
	if events > t.IncludedEvents {
		q.add(band(t.IncludedEvents, events, t.RateCentsPerMillion))
	}
	return q
}

// band rounds half up on its own line, so the lines a customer sees sum to the total.
func band(from, to, centsPerMillion int64) Line {
	n := to - from
	return Line{
		Description:     eventRange(from+1, to),
		Events:          n,
		CentsPerMillion: centsPerMillion,
		AmountCents:     (n*centsPerMillion + 500_000) / 1_000_000,
	}
}

func (q *Quote) add(l Line) {
	q.Lines = append(q.Lines, l)
	q.TotalCents += l.AmountCents
}

func eventRange(from, to int64) string {
	if from == to {
		return "event " + comma(from)
	}
	return "events " + comma(from) + " to " + comma(to)
}

func comma(v int64) string {
	s := strconv.FormatInt(v, 10)
	var b strings.Builder
	for i := range len(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
