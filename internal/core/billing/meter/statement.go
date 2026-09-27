package meter

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

// carry is what a finished window still owes each tier: its final split minus what
// was already stated for it, never negative. It lands in the same tier next period,
// billed at the rate it earned.
func carry(final, stated []int64) []int64 {
	out := make([]int64, len(final))
	for k := range final {
		out[k] = max(0, final[k]-stated[k])
	}
	return out
}

// maxEach keeps a statement from going down: a max meter would ignore a lower value
// anyway, and the ledger must match what the provider holds.
func maxEach(a, b []int64) []int64 {
	out := make([]int64, len(a))
	for k := range a {
		out[k] = max(a[k], b[k])
	}
	return out
}

func sumEach(a, b []int64) []int64 {
	out := make([]int64, len(a))
	for k := range a {
		out[k] = a[k] + b[k]
	}
	return out
}

// eventID is derived from exactly what is stated, so a statement can be traced from
// the provider's event log to its ledger row. The provider does not deduplicate on
// it — a repeat is stored twice — so what makes a re-send harmless is the max meter,
// which ignores a value it already holds. Hashed because the provider documents no
// length limit and six counts written out run long.
func eventID(orgID string, periodStart time.Time, tiers []int64) string {
	var b strings.Builder
	b.WriteString(orgID)
	b.WriteString("|")
	b.WriteString(strconv.FormatInt(periodStart.Unix(), 10))
	for _, n := range tiers {
		b.WriteString("|")
		b.WriteString(strconv.FormatInt(n, 10))
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "pug_" + hex.EncodeToString(sum[:])[:32]
}
