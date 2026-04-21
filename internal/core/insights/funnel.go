package insights

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	insightsv1 "github.com/fivebitsio/cotton/internal/gen/proto/shared/insights/v1"
	"github.com/fivebitsio/cotton/internal/slogx"
)

// FunnelUserEvents holds per-user event data from the array-based funnel query.
// Times and StepMatches are parallel arrays sorted by occur_time ASC.
// StepMatches[i] is the step index (0-based) that event i matches via multiIf tagging.
// Breakdowns holds the per-dimension breakdown values for this user (empty when no breakdown).
type FunnelUserEvents struct {
	DistinctID  string
	Times       []time.Time
	StepMatches []int64
	Breakdowns  []string
}

// ComputeFunnelTiming does greedy sequential step matching per user and aggregates
// counts + average time-to-convert per step, grouped by breakdown combination.
//
// For each user, it walks events in time order and greedily matches: the first event
// with step_match == current_step advances the funnel. The conversion window (in seconds)
// is enforced from step 0's timestamp — if a later step exceeds the window, the user's
// funnel is truncated there. A windowSec of 0 means no window constraint.
//
// Users with no breakdowns (empty Breakdowns slice) are all grouped together, producing
// a single series — the same behaviour as before breakdowns were introduced.
func ComputeFunnelTiming(ctx context.Context, users []FunnelUserEvents, kinds []string, windowSec int64, numBreakdowns int) ([]FunnelRow, error) {
	numSteps := len(kinds)
	if numSteps == 0 {
		err := fmt.Errorf("kinds must not be empty")
		slog.ErrorContext(ctx, "insights: compute funnel timing failed", slogx.Error(err))
		return nil, err
	}

	type stepAcc struct {
		count    int64
		totalSec float64
	}

	// Validate uniform breakdown length across all users.
	expectedBDs := numBreakdowns
	for _, u := range users {
		if len(u.Breakdowns) != expectedBDs {
			err := fmt.Errorf("user %s: has %d breakdowns but expected %d",
				u.DistinctID, len(u.Breakdowns), expectedBDs)
			slog.ErrorContext(ctx, "insights: compute funnel timing failed", slogx.Error(err),
				slog.String("userID", u.DistinctID))
			return nil, err
		}
	}

	var orderedKeys []string
	breakdownsByKey := map[string][]string{}
	accsByKey := map[string][]stepAcc{}

	for _, u := range users {
		if len(u.Times) != len(u.StepMatches) {
			err := fmt.Errorf("user %s: mismatched array lengths (times=%d, step_matches=%d)",
				u.DistinctID, len(u.Times), len(u.StepMatches))
			slog.ErrorContext(ctx, "insights: compute funnel timing failed", slogx.Error(err),
				slog.String("userID", u.DistinctID))
			return nil, err
		}

		key := breakdownKey(u.Breakdowns)
		if _, ok := accsByKey[key]; !ok {
			orderedKeys = append(orderedKeys, key)
			breakdownsByKey[key] = slices.Clone(u.Breakdowns)
			accsByKey[key] = make([]stepAcc, numSteps)
		}
		accs := accsByKey[key]

		stepTimes := make([]time.Time, numSteps)
		matched := 0

		for j := range u.Times {
			if matched >= numSteps {
				break
			}
			if u.StepMatches[j] != int64(matched) {
				continue
			}
			t := u.Times[j]

			// Enforce conversion window from step 0.
			if matched > 0 && windowSec > 0 {
				if t.Sub(stepTimes[0]).Seconds() > float64(windowSec) {
					break
				}
			}

			stepTimes[matched] = t
			matched++
		}

		for s := range matched {
			accs[s].count++
			if s > 0 {
				accs[s].totalSec += stepTimes[s].Sub(stepTimes[s-1]).Seconds()
			}
		}
	}

	// Seed zero-count rows if no users produced any keys — ensures the timing path
	// returns one row per step (like windowFunnel's countIf=0 rows).
	// Checked after the loop so users with all-empty breakdown values correctly produce
	// a real key during the loop, avoiding a spurious duplicate entry for the empty-breakdown key.
	if len(orderedKeys) == 0 {
		slog.DebugContext(ctx, "funnel timing: no matching users, returning zero-count rows",
			slog.Int("steps", numSteps))
		emptyBDs := make([]string, expectedBDs)
		key := breakdownKey(emptyBDs)
		orderedKeys = append(orderedKeys, key)
		breakdownsByKey[key] = emptyBDs
		accsByKey[key] = make([]stepAcc, numSteps)
	}

	var rows []FunnelRow
	for _, key := range orderedKeys {
		accs := accsByKey[key]
		bds := breakdownsByKey[key]
		for i := range numSteps {
			var avgTime float64
			if i > 0 && accs[i].count > 0 {
				avgTime = accs[i].totalSec / float64(accs[i].count)
			}
			rows = append(rows, FunnelRow{
				StepIndex:         int64(i),
				EventKind:         kinds[i],
				Breakdowns:        bds,
				Value:             float64(accs[i].count),
				AvgConvertSeconds: avgTime,
			})
		}
	}
	return rows, nil
}

// EffectiveWindowSec returns the conversion window in seconds for funnel queries.
// If the request specifies a non-zero value, it is used directly.
// Otherwise, defaults to the full time range duration.
func EffectiveWindowSec(req *insightsv1.QueryRequest) int64 {
	if s := int64(req.GetConversionWindowSeconds()); s > 0 {
		return s
	}
	dur := int64(req.GetTimeRange().GetTo().AsTime().Sub(req.GetTimeRange().GetFrom().AsTime()).Seconds())
	if dur <= 0 {
		return 0
	}
	return dur
}
