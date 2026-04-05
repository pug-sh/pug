package insights

import (
	"fmt"
	"time"

	insightsv1 "github.com/fivebitsio/cotton/internal/gen/proto/shared/insights/v1"
)

// FunnelUserEvents holds per-user event data from the array-based funnel query.
// Times and StepMatches are parallel arrays sorted by occur_time ASC.
// StepMatches[i] is the step index (0-based) that event i matches via multiIf tagging.
type FunnelUserEvents struct {
	DistinctID  string
	Times       []time.Time
	StepMatches []int64
}

// ComputeFunnelTiming does greedy sequential step matching per user and aggregates
// counts + average time-to-convert per step.
//
// For each user, it walks events in time order and greedily matches: the first event
// with step_match == current_step advances the funnel. The conversion window (in seconds)
// is enforced from step 0's timestamp — if a later step exceeds the window, the user's
// funnel is truncated there. A windowSec of 0 means no window constraint.
func ComputeFunnelTiming(users []FunnelUserEvents, kinds []string, windowSec int64) ([]FunnelRow, error) {
	numSteps := len(kinds)
	if numSteps == 0 {
		return nil, fmt.Errorf("kinds must not be empty")
	}

	type stepAcc struct {
		count    int64
		totalSec float64
	}
	accs := make([]stepAcc, numSteps)

	for _, u := range users {
		if len(u.Times) != len(u.StepMatches) {
			return nil, fmt.Errorf("user %s: mismatched array lengths (times=%d, step_matches=%d)",
				u.DistinctID, len(u.Times), len(u.StepMatches))
		}

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

	rows := make([]FunnelRow, numSteps)
	for i := range numSteps {
		var avgTime float64
		if i > 0 && accs[i].count > 0 {
			avgTime = accs[i].totalSec / float64(accs[i].count)
		}
		rows[i] = FunnelRow{
			StepIndex:         int64(i),
			EventKind:         kinds[i],
			Value:             float64(accs[i].count),
			AvgConvertSeconds: avgTime,
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
	return int64(req.GetTimeRange().GetTo().AsTime().Sub(req.GetTimeRange().GetFrom().AsTime()).Seconds())
}
