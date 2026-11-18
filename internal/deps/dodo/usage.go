package dodo

import (
	"context"
	"fmt"
	"strconv"

	"github.com/dodopayments/dodopayments-go"
	"github.com/dodopayments/dodopayments-go/shared"
	corebilling "github.com/pug-sh/pug/internal/core/billing"
)

// usageEventName is the event every tier meter is configured on; each meter takes
// the max of its own tier's metadata key. Both names stop here: above the seam a
// tier is an index.
const usageEventName = "pug.usage"

func tierKey(k int) string { return "t" + strconv.Itoa(k+1) }

var _ corebilling.UsageMeter = (*Client)(nil)

// IngestUsage states one customer's per-tier counts as a single event carrying one
// metadata key per tier. The SDK's retries are safe here, unlike on a charge: every
// meter takes the max, so a repeat changes nothing. That is the guard pug relies
// on. Dodo documents a repeated event id as ignored, yet in test mode a replay was
// ingested again and listed twice until a later merge collapsed it.
//
// A 2xx is not proof Dodo holds the event: it answers ingested_count 0 for one it
// refused, such as a timestamp more than an hour old or five minutes ahead. A 0 can
// also be a repeat it ignored, which it holds already, so a 0 is settled by reading
// the event back.
func (c *Client) IngestUsage(ctx context.Context, s corebilling.UsageStatement) error {
	meta := make(map[string]dodopayments.EventInputMetadataUnionParam, len(s.TierEvents))
	for k, n := range s.TierEvents {
		meta[tierKey(k)] = shared.UnionFloat(float64(n))
	}
	res, err := c.api.UsageEvents.Ingest(ctx, dodopayments.UsageEventIngestParams{
		Events: dodopayments.F([]dodopayments.EventInputParam{{
			CustomerID: dodopayments.F(s.CustomerID),
			EventID:    dodopayments.F(s.EventID),
			EventName:  dodopayments.F(usageEventName),
			Metadata:   dodopayments.F(meta),
			Timestamp:  dodopayments.F(s.At.UTC()),
		}}),
	})
	if err != nil {
		return fmt.Errorf("dodo: ingest usage: %w", err)
	}
	switch res.IngestedCount {
	case 1:
		return nil
	case 0:
		if _, err := c.api.UsageEvents.Get(ctx, s.EventID); err != nil {
			if notFound(err) {
				return fmt.Errorf("dodo: ingest usage: event %s was accepted but not ingested, and Dodo does not hold it", s.EventID)
			}
			return fmt.Errorf("dodo: ingest usage: confirm event %s: %w", s.EventID, err)
		}
		return nil
	}
	return fmt.Errorf("dodo: ingest usage: one event sent, %d ingested", res.IngestedCount)
}

// VerifyMetering reads the provider's meters and each product and checks them
// against what IngestUsage sends. A missing meter would bill its tier at zero, a
// free threshold would apply pug's allowance twice, and any other meter attached —
// a sum over a tier's key, or a key past the plan's last tier — would bill what pug
// never meant, all silently.
func (c *Client) VerifyMetering(ctx context.Context, tiers int, productIDs []string) error {
	byKey := map[string]string{}
	// Page 0 named explicitly: Dodo counts pages from 0, and the SDK's pager, finding
	// no page number on the first request, assumes it was 1 and asks for 2 next.
	iter := c.api.Meters.ListAutoPaging(ctx, dodopayments.MeterListParams{
		PageNumber: dodopayments.F(int64(0)),
		PageSize:   dodopayments.F(int64(100)),
	})
	for iter.Next() {
		m := iter.Current()
		// Only a max meter is a tier's meter. Any other on the same key is caught
		// below if a product attaches it.
		if m.EventName != usageEventName || m.Aggregation.Type != dodopayments.MeterAggregationTypeMax {
			continue
		}
		// The same meter on two pages is one meter; two meters on one key is not.
		if other, dup := byKey[m.Aggregation.Key]; dup && other != m.ID {
			return misconfiguredf("meters %s and %s both take the max of %s.%s", other, m.ID, usageEventName, m.Aggregation.Key)
		}
		byKey[m.Aggregation.Key] = m.ID
	}
	if err := iter.Err(); err != nil {
		return fmt.Errorf("dodo: list meters: %w", err)
	}
	want := make([]string, tiers)
	tierOf := make(map[string]int, tiers)
	for k := range want {
		id, ok := byKey[tierKey(k)]
		if !ok {
			return misconfiguredf("no max meter on %s.%s for tier %d", usageEventName, tierKey(k), k+1)
		}
		want[k], tierOf[id] = id, k
	}
	for _, productID := range productIDs {
		p, err := c.api.Products.Get(ctx, productID)
		if err != nil {
			if notFound(err) {
				return misconfiguredf("product %s does not exist", productID)
			}
			return fmt.Errorf("dodo: get product %s: %w", productID, err)
		}
		price, ok := p.Price.AsUnion().(dodopayments.PriceUsageBasedPrice)
		if !ok {
			return misconfiguredf("product %s is not usage-priced", productID)
		}
		if price.FixedPrice < corebilling.MinFixedFeeCents {
			return misconfiguredf("product %s has a fixed fee of %d cents; at least %d is needed to collect a quiet month",
				productID, price.FixedPrice, corebilling.MinFixedFeeCents)
		}
		attached := make(map[string]int64, len(price.Meters))
		for _, m := range price.Meters {
			if _, ours := tierOf[m.MeterID]; !ours {
				return misconfiguredf("product %s attaches meter %s, which is none of the %d tier meters; it would bill what pug never states to it",
					productID, m.MeterID, tiers)
			}
			attached[m.MeterID] = m.FreeThreshold
		}
		for k, id := range want {
			threshold, ok := attached[id]
			if !ok {
				return misconfiguredf("product %s does not attach tier %d's meter %s; that tier would bill nothing", productID, k+1, id)
			}
			if threshold != 0 {
				return misconfiguredf("product %s gives tier %d's meter a free threshold of %d; pug's allowance would apply twice",
					productID, k+1, threshold)
			}
		}
	}
	return nil
}

// misconfiguredf is a VerifyMetering finding: the provider was read, and would bill
// a statement wrongly.
func misconfiguredf(format string, args ...any) error {
	return fmt.Errorf("dodo: %w: %s", corebilling.ErrMeteringMisconfigured, fmt.Sprintf(format, args...))
}
