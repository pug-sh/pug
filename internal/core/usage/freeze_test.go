package usage_test

import (
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	coreusage "github.com/pug-sh/pug/internal/core/usage"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
)

func TestMeterProjectCountsOneProject(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	ch := testutil.SetupClickHouse(t)
	svc := f.svc.WithClickHouse(ch.Conn)

	day := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	other := seedProject(t, f.w, f.orgID)
	insertEvent(t, ch.Conn, f.projectID, uuid.NewString(), day.Add(time.Hour))
	insertEvent(t, ch.Conn, f.projectID, uuid.NewString(), day.Add(25*time.Hour))
	insertEvent(t, ch.Conn, other, uuid.NewString(), day.Add(time.Hour))

	got, err := svc.MeterProject(t.Context(), f.projectID, day, day.AddDate(0, 0, 2))
	if err != nil {
		t.Fatalf("MeterProject: %v", err)
	}
	slices.SortFunc(got, func(a, b coreusage.DailyUsage) int { return a.Day.Compare(b.Day) })
	want := []coreusage.DailyUsage{
		{ProjectID: f.projectID, Day: day, EventCount: 1},
		{ProjectID: f.projectID, Day: day.AddDate(0, 0, 1), EventCount: 1},
	}
	if !slices.EqualFunc(got, want, sameCell) {
		t.Errorf("MeterProject = %+v, want %+v", got, want)
	}
}

// The meter skips a hidden project's days, so only the freeze can write them.
func TestFreezeWritesAHiddenProjectsDays(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	ctx := t.Context()
	day1 := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	day2 := day1.AddDate(0, 0, 1)

	if err := f.svc.RecordDailyUsage(ctx, []coreusage.DailyUsage{
		{ProjectID: f.projectID, Day: day1, EventCount: 5},
	}); err != nil {
		t.Fatalf("RecordDailyUsage: %v", err)
	}
	if err := f.w.HideProject(ctx, f.projectID); err != nil {
		t.Fatalf("HideProject: %v", err)
	}
	frozen := []coreusage.DailyUsage{
		{ProjectID: f.projectID, Day: day1, EventCount: 9},
		{ProjectID: f.projectID, Day: day2, EventCount: 2},
	}
	if err := coreusage.FreezeDailyUsageInTx(ctx, dbwrite.New(f.pg.PgW), f.orgID, f.projectID, frozen); err != nil {
		t.Fatalf("FreezeDailyUsageInTx: %v", err)
	}

	got, err := f.svc.ListDailyUsage(ctx, f.orgID, day1, day2.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("ListDailyUsage: %v", err)
	}
	if !slices.EqualFunc(got, frozen, sameCell) {
		t.Errorf("stored days = %+v, want %+v", got, frozen)
	}
}

func sameCell(a, b coreusage.DailyUsage) bool {
	return a.ProjectID == b.ProjectID && a.Day.Equal(b.Day) && a.EventCount == b.EventCount
}
