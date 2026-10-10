package retention

import (
	"testing"
	"time"
)

func TestStateNext(t *testing.T) {
	t0 := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	waiting := state{days: 1825, pendingDays: 365, pendingSince: t0}
	held := state{days: 1825, pendingDays: 365}
	expired := state{days: 1825, pendingDays: 365, pendingSince: t0, expiredBy: "ops/1"}
	for _, tc := range []struct {
		name   string
		cur    state
		days   int64
		lapsed bool
		now    time.Time
		want   state
	}{
		{"no row and no length", state{}, 0, false, t0, state{}},
		{"no row counts as none, so a first length waits", state{}, 365, false, t0,
			state{pendingDays: 365, pendingSince: t0}},
		{"a lapsed first length waits for an expire", state{}, 365, true, t0, state{pendingDays: 365}},
		{"an unchanged length", state{days: 365}, 365, false, t0, state{days: 365}},
		{"a longer length applies at once", state{days: 365}, 1825, false, t0, state{days: 1825}},
		{"none applies at once", state{days: 365}, 0, false, t0, state{}},
		{"a shorter length waits", state{days: 1825}, 365, false, t0, waiting},

		{"the wait runs", waiting, 365, false, t0.Add(wait - time.Second), waiting},
		{"then the shorter length applies", waiting, 365, false, t0.Add(wait), state{days: 365}},
		{"a new shorter length restarts the wait", waiting, 90, false, t0.Add(wait),
			state{days: 1825, pendingDays: 90, pendingSince: t0.Add(wait)}},
		{"an equal length clears the wait", waiting, 1825, false, t0, state{days: 1825}},

		{"a held drop waits for no clock", held, 365, true, t0.Add(10 * wait), held},
		{"a held drop goes once the org pays again", held, 1825, false, t0, state{days: 1825}},
		{"an expired drop runs its wait", expired, 365, false, t0.Add(wait - time.Second), expired},
		{"then applies, and the expire goes with it", expired, 365, false, t0.Add(wait), state{days: 365}},
		{"a new drop replaces an expired one", expired, 90, false, t0,
			state{days: 1825, pendingDays: 90, pendingSince: t0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cur.next(tc.days, tc.lapsed, tc.now); got != tc.want {
				t.Errorf("next(%d) = %+v, want %+v", tc.days, got, tc.want)
			}
		})
	}
}

// The pass asks whether a drop is lapsed only when next would start a wait.
func TestStateDrops(t *testing.T) {
	waiting := state{days: 1825, pendingDays: 365}
	for _, tc := range []struct {
		cur  state
		days int64
		want bool
	}{
		{state{}, 0, false},
		{state{}, 365, true},
		{state{days: 365}, 1825, false},
		{state{days: 1825}, 365, true},
		{waiting, 365, false},
		{waiting, 90, true},
		{waiting, 1825, false},
	} {
		if got := tc.cur.drops(tc.days); got != tc.want {
			t.Errorf("%+v.drops(%d) = %t, want %t", tc.cur, tc.days, got, tc.want)
		}
	}
}
