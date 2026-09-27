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
// meter takes the max, so a repeat changes nothing. That is the only guard — Dodo
// stores a repeated event id rather than dropping it.
func (c *Client) IngestUsage(ctx context.Context, s corebilling.UsageStatement) error {
	meta := make(map[string]dodopayments.EventInputMetadataUnionParam, len(s.TierEvents))
	for k, n := range s.TierEvents {
		meta[tierKey(k)] = shared.UnionFloat(float64(n))
	}
	_, err := c.api.UsageEvents.Ingest(ctx, dodopayments.UsageEventIngestParams{
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
	return nil
}

// VerifyMetering reads the provider's meters and each product and checks them
// against what IngestUsage sends. A missing meter would bill its tier at zero and a
// free threshold would apply pug's allowance twice, both silently.
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
		if m.EventName != usageEventName || m.Aggregation.Type != dodopayments.MeterAggregationTypeMax {
			continue
		}
		// The same meter on two pages is one meter; two meters on one key is not.
		if other, dup := byKey[m.Aggregation.Key]; dup && other != m.ID {
			return fmt.Errorf("dodo: meters %s and %s both take the max of %s.%s", other, m.ID, usageEventName, m.Aggregation.Key)
		}
		byKey[m.Aggregation.Key] = m.ID
	}
	if err := iter.Err(); err != nil {
		return fmt.Errorf("dodo: list meters: %w", err)
	}
	want := make([]string, tiers)
	for k := range want {
		id, ok := byKey[tierKey(k)]
		if !ok {
			return fmt.Errorf("dodo: no max meter on %s.%s for tier %d", usageEventName, tierKey(k), k+1)
		}
		want[k] = id
	}
	for _, productID := range productIDs {
		p, err := c.api.Products.Get(ctx, productID)
		if err != nil {
			return fmt.Errorf("dodo: get product %s: %w", productID, err)
		}
		price, ok := p.Price.AsUnion().(dodopayments.PriceUsageBasedPrice)
		if !ok {
			return fmt.Errorf("dodo: product %s is not usage-priced", productID)
		}
		if price.FixedPrice < corebilling.MinFixedFeeCents {
			return fmt.Errorf("dodo: product %s has a fixed fee of %d cents; at least %d is needed to collect a quiet month",
				productID, price.FixedPrice, corebilling.MinFixedFeeCents)
		}
		attached := make(map[string]int64, len(price.Meters))
		for _, m := range price.Meters {
			attached[m.MeterID] = m.FreeThreshold
		}
		for k, id := range want {
			threshold, ok := attached[id]
			if !ok {
				return fmt.Errorf("dodo: product %s does not attach tier %d's meter %s; that tier would bill nothing", productID, k+1, id)
			}
			if threshold != 0 {
				return fmt.Errorf("dodo: product %s gives tier %d's meter a free threshold of %d; pug's allowance would apply twice",
					productID, k+1, threshold)
			}
		}
	}
	return nil
}
