package insights

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/fivebitsio/cotton/internal/deps/telemetry"
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

// ComputeFunnelTiming does greedy sequential step matching per user and aggregates per-step
// counts plus conversion-time statistics (average, median, p95, and an 8-bucket distribution),
// grouped by breakdown combination.
//
// For each user, it walks events in time order and greedily matches: the first event
// with step_match == current_step advances the funnel. The conversion window (in seconds)
// is enforced from step 0's timestamp — if a later step exceeds the window, the user's
// funnel is truncated there. A windowSec of 0 means no window constraint; negative
// values are rejected as invalid.
//
// projectID is included in log records for operability — pass an empty string when
// calling from contexts that don't have one (e.g. unit tests).
//
// Users with no breakdowns (empty Breakdowns slice) are all grouped together,
// producing a single series.
func ComputeFunnelTiming(ctx context.Context, projectID string, users []FunnelUserEvents, kinds []string, windowSec int64, numBreakdowns int) ([]FunnelRow, error) {
	numSteps := len(kinds)
	if numSteps == 0 {
		err := errors.New("kinds must not be empty")
		slog.ErrorContext(ctx, "ComputeFunnelTiming: invalid input", slogx.Error(err),
			slog.String("projectID", projectID))
		telemetry.RecordError(ctx, err)
		return nil, err
	}
	if windowSec < 0 {
		err := fmt.Errorf("windowSec must be >= 0, got %d", windowSec)
		slog.ErrorContext(ctx, "ComputeFunnelTiming: invalid input", slogx.Error(err),
			slog.String("projectID", projectID))
		telemetry.RecordError(ctx, err)
		return nil, err
	}

	type stepAcc struct {
		count int64
		total time.Duration
		times []time.Duration // per-user delta-from-previous-step durations; appended only when s>0 and the user converted to step s.
	}

	// Validate uniform breakdown length across all users.
	expectedBDs := numBreakdowns
	for _, u := range users {
		if len(u.Breakdowns) != expectedBDs {
			err := fmt.Errorf("user %s: has %d breakdowns but expected %d",
				u.DistinctID, len(u.Breakdowns), expectedBDs)
			slog.ErrorContext(ctx, "ComputeFunnelTiming: breakdown length mismatch", slogx.Error(err),
				slog.String("projectID", projectID), slog.String("distinct_id", u.DistinctID))
			telemetry.RecordError(ctx, err)
			return nil, err
		}
	}

	var orderedKeys []string
	breakdownsByKey := map[string][]string{}
	accsByKey := map[string][]stepAcc{}

	windowDur := time.Duration(windowSec) * time.Second

	for _, u := range users {
		if len(u.Times) != len(u.StepMatches) {
			err := fmt.Errorf("user %s: mismatched array lengths (times=%d, step_matches=%d)",
				u.DistinctID, len(u.Times), len(u.StepMatches))
			slog.ErrorContext(ctx, "ComputeFunnelTiming: array length mismatch", slogx.Error(err),
				slog.String("projectID", projectID), slog.String("distinct_id", u.DistinctID))
			telemetry.RecordError(ctx, err)
			return nil, err
		}

		key := breakdownKey(u.Breakdowns)
		if _, ok := accsByKey[key]; !ok {
			orderedKeys = append(orderedKeys, key)
			// Defensive clone: u.Breakdowns is held in the result rows, so a later caller
			// mutating it would otherwise corrupt the map values.
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
			if matched > 0 && windowDur > 0 {
				if t.Sub(stepTimes[0]) > windowDur {
					break
				}
			}

			stepTimes[matched] = t
			matched++
		}

		for s := range matched {
			accs[s].count++
			if s > 0 {
				delta := stepTimes[s].Sub(stepTimes[s-1])
				if delta < 0 {
					// Defense against upstream regressions that drop the SQL-side arraySort.
					// time.Time.Sub on a sorted-ascending slice cannot produce negative deltas.
					err := fmt.Errorf("user %s: negative delta at step %d (events not sorted)", u.DistinctID, s)
					slog.ErrorContext(ctx, "ComputeFunnelTiming: negative delta", slogx.Error(err),
						slog.String("projectID", projectID), slog.String("distinct_id", u.DistinctID), slog.Int("step", s))
					telemetry.RecordError(ctx, err)
					return nil, err
				}
				accs[s].total += delta
				accs[s].times = append(accs[s].times, delta)
			}
		}
	}

	// Seed zero-count rows if no users produced any keys — ensures the timing path
	// returns one row per step (like windowFunnel's countIf=0 rows).
	// Checked after the loop so users with all-empty breakdown values correctly produce
	// a real key during the loop, avoiding a spurious duplicate entry for the empty-breakdown key.
	if len(orderedKeys) == 0 {
		slog.InfoContext(ctx, "funnel timing: no matching users, returning zero-count rows",
			slog.String("projectID", projectID),
			slog.Int("steps", numSteps),
			slog.Int("users", len(users)))
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
			row := FunnelRow{
				StepIndex:  int64(i),
				EventKind:  kinds[i],
				Breakdowns: bds,
				Value:      float64(accs[i].count),
			}
			if i > 0 {
				timing := newStepTiming()
				if accs[i].count > 0 {
					timing.Avg = accs[i].total / time.Duration(accs[i].count)
				}
				if len(accs[i].times) > 0 {
					slices.Sort(accs[i].times)
					// Bool returns are safe to discard here: medianSorted only returns false on
					// empty input (gated by len > 0 above), and percentileSorted only returns
					// false on empty/NaN/out-of-range p (literal 0.95 is in (0, 1]).
					timing.Median, _ = medianSorted(accs[i].times)
					timing.P95, _ = percentileSorted(accs[i].times, 0.95)
					timing.Distribution = distributionCountsSorted(accs[i].times)
				}
				row.Timing = timing
			}
			rows = append(rows, row)
		}
	}
	return rows, nil
}

// EffectiveWindowSec returns the conversion window in seconds for funnel queries.
// If the request specifies a positive Duration, it is used directly.
// Otherwise (absent, zero, or negative), defaults to the full time range duration.
// Returns an error if no usable conversion_window is set and the time range is empty or inverted —
// this surfaces what would otherwise silently degrade to "no window constraint".
func EffectiveWindowSec(req *insightsv1.QueryRequest) (int64, error) {
	if d := req.GetConversionWindow().AsDuration(); d > 0 {
		s := int64(d.Seconds())
		if s <= 0 {
			// Defense in depth for non-RPC callers: protovalidate's gte: 1s + whole_seconds CEL
			// catch this on the wire, but workers/scripts/tests bypass the interceptor.
			return 0, fmt.Errorf("conversion_window must be at least 1s, got %v", d)
		}
		return s, nil
	}
	tr := req.GetTimeRange()
	dur := tr.GetTo().AsTime().Sub(tr.GetFrom().AsTime())
	if dur <= 0 {
		return 0, fmt.Errorf("time_range is empty or inverted: from=%v to=%v",
			tr.GetFrom().AsTime(), tr.GetTo().AsTime())
	}
	return int64(dur.Seconds()), nil
}
