package usage

import (
	"testing"
	"time"

	"connectrpc.com/connect"
	coreusage "github.com/pug-sh/pug/internal/core/usage"
	usagev1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/usage/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
	"github.com/rs/xid"
)

func seedOrgProject(t *testing.T, w *dbwrite.Queries) (orgID, projectID string) {
	t.Helper()

	org, err := w.CreateOrg(t.Context(), dbwrite.CreateOrgParams{
		ID: xid.New().String(), DisplayName: "acme",
	})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	id := xid.New().String()
	project, err := w.CreateProject(t.Context(), dbwrite.CreateProjectParams{
		ID: id, OrgID: org.ID, DisplayName: "project-" + id,
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	return org.ID, project.ID
}

// The wire shape is where "never metered" becomes visible to a client, and it is
// the one thing the subsystem exists to get right: a count and its provenance are
// set together or not at all, so no client can read a zero the server cannot back.
func TestGetUsageOmitsBothFieldsUntilMetered(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	svc := coreusage.NewService(pg.PgRO, pg.PgW)
	orgID, projectID := seedOrgProject(t, dbwrite.New(pg.PgW))
	srv := NewServer(svc)

	resp, err := srv.GetUsage(t.Context(), connect.NewRequest(&usagev1.GetUsageRequest{
		OrgId: &orgID,
	}))
	if err != nil {
		t.Fatalf("GetUsage: %v", err)
	}
	if resp.Msg.UsageComputedAt != nil {
		t.Errorf("usage_computed_at = %v on an unmetered org, want absent", resp.Msg.UsageComputedAt)
	}
	if resp.Msg.UsedEvents != nil {
		t.Errorf("used_events = %d on an unmetered org, want absent — a client would render it as 0",
			resp.Msg.GetUsedEvents())
	}
	if resp.Msg.GetPeriodStart() == nil || resp.Msg.GetPeriodEnd() == nil {
		t.Error("period bounds should be returned regardless of metering")
	}

	// Metered, and it really is zero: both fields present.
	now := time.Now().UTC()
	start, end := coreusage.CalendarMonth(now)
	if _, err := svc.RefreshPeriodUsage(t.Context(), orgID, start, end); err != nil {
		t.Fatalf("RefreshPeriodUsage: %v", err)
	}

	resp, err = srv.GetUsage(t.Context(), connect.NewRequest(&usagev1.GetUsageRequest{OrgId: &orgID}))
	if err != nil {
		t.Fatalf("GetUsage (metered): %v", err)
	}
	if resp.Msg.UsageComputedAt == nil {
		t.Error("usage_computed_at is absent after a metering pass")
	}
	if resp.Msg.UsedEvents == nil {
		t.Fatal("used_events is absent after a metering pass; a metered zero must be reported as 0")
	}
	if resp.Msg.GetUsedEvents() != 0 {
		t.Errorf("used_events = %d, want 0", resp.Msg.GetUsedEvents())
	}

	// And a real count round-trips with its daily series.
	day := coreusage.FloorDayUTC(now)
	if err := svc.RecordDailyUsage(t.Context(), []coreusage.DailyUsage{
		{Day: day, EventCount: 7, ProjectID: projectID},
	}); err != nil {
		t.Fatalf("RecordDailyUsage: %v", err)
	}
	if _, err := svc.RefreshPeriodUsage(t.Context(), orgID, start, end); err != nil {
		t.Fatalf("RefreshPeriodUsage: %v", err)
	}

	resp, err = getUsage(t, srv, orgID)
	if err != nil {
		t.Fatalf("GetUsage (counted): %v", err)
	}
	if resp.Msg.GetUsedEvents() != 7 {
		t.Errorf("used_events = %d, want 7", resp.Msg.GetUsedEvents())
	}
	if len(resp.Msg.GetDaily()) != 1 || resp.Msg.GetDaily()[0].GetEventCount() != 7 {
		t.Errorf("daily = %+v, want one cell of 7", resp.Msg.GetDaily())
	}
}

// An org whose usage the meter has not reached this period still reports the
// stamp from its last one, so a month rollover does not read as "never metered".
func TestGetUsageKeepsTheStampAcrossAMonthRollover(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	svc := coreusage.NewService(pg.PgRO, pg.PgW)
	orgID, _ := seedOrgProject(t, dbwrite.New(pg.PgW))

	// Meter only the *previous* month, leaving the current period rowless. Stepped
	// back from the 1st, not from today: AddDate normalizes Feb 31 to March.
	currentStart, _ := coreusage.CalendarMonth(time.Now().UTC())
	prevStart, prevEnd := currentStart.AddDate(0, -1, 0), currentStart
	if _, err := svc.RefreshPeriodUsage(t.Context(), orgID, prevStart, prevEnd); err != nil {
		t.Fatalf("RefreshPeriodUsage: %v", err)
	}

	resp, err := getUsage(t, NewServer(svc), orgID)
	if err != nil {
		t.Fatalf("GetUsage: %v", err)
	}
	if resp.Msg.UsageComputedAt == nil {
		t.Error("usage_computed_at is absent, so every dashboard reads 'never metered' on the 1st")
	}
	if resp.Msg.GetUsedEvents() != 0 {
		t.Errorf("used_events = %d, want 0 for a period the meter has not reached", resp.Msg.GetUsedEvents())
	}
}

func getUsage(t *testing.T, srv *Server, orgID string) (*connect.Response[usagev1.GetUsageResponse], error) {
	t.Helper()
	return srv.GetUsage(t.Context(), connect.NewRequest(&usagev1.GetUsageRequest{OrgId: &orgID}))
}
