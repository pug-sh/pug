package purge_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/rs/xid"

	"github.com/pug-sh/pug/internal/core/projects"
	"github.com/pug-sh/pug/internal/core/purge"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
)

// A table added by a migration and missing here would keep a deleted project's
// rows forever.
func TestCheckCoversEveryProjectTable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	ch := testutil.SetupClickHouse(t)
	svc := purge.NewService(pg.PgW, ch.Conn)

	state, err := svc.Check(t.Context(), purge.Filter{ProjectID: xid.New().String()})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	var got []string
	for _, tb := range state {
		got = append(got, tb.Name)
	}
	slices.Sort(got)
	if want := projectTables(t, ch); !slices.Equal(got, want) {
		t.Errorf("purge tables = %v, want every table with a project_id column: %v", got, want)
	}
}

func TestStartQueuesOneDeletePerTable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	ch := testutil.SetupClickHouse(t)
	ctx := t.Context()
	svc := purge.NewService(pg.PgW, ch.Conn)

	// No projects rows, so none of them is live.
	a := purge.Filter{ProjectID: xid.New().String()}
	b := purge.Filter{ProjectID: xid.New().String()}
	kept := xid.New().String()
	now := time.Now().UTC()
	for _, id := range []string{a.ProjectID, b.ProjectID, kept} {
		seedClickHouse(t, ch, id, now)
	}
	keptRows := map[string]uint64{}
	for _, table := range projectTables(t, ch) {
		keptRows[table] = rowCount(t, ch, table, kept)
	}

	var deleted []string
	for _, table := range projectTables(t, ch) {
		if table != "property_keys_profile_current" {
			deleted = append(deleted, table)
		}
	}

	setMerges(t, ch, false)
	queued, err := svc.Start(ctx, a)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	slices.Sort(queued)
	if !slices.Equal(queued, deleted) {
		t.Fatalf("Start queued %v, want %v", queued, deleted)
	}
	again, err := svc.Start(ctx, a)
	if err != nil || len(again) != 0 {
		t.Errorf("second Start = %v, %v; want nothing while the first deletes run", again, err)
	}
	for _, table := range deleted {
		if n := queuedDeletes(t, ch, a.ProjectID)[table]; n != 1 {
			t.Errorf("%s has %d deletes for the project, want 1", table, n)
		}
	}
	state, err := svc.Check(ctx, a)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	for _, tb := range state {
		if !tb.HasRows || tb.Deleting == tb.Rebuilt {
			t.Errorf("while deleting: %+v", tb)
		}
	}

	queuedB, err := svc.Start(ctx, b)
	if err != nil {
		t.Fatalf("Start(b): %v", err)
	}
	slices.Sort(queuedB)
	if !slices.Equal(queuedB, deleted) {
		t.Errorf("Start(b) queued %v behind a's deletes, want %v", queuedB, deleted)
	}

	setMerges(t, ch, true)
	waitForDeletes(t, ch)
	for _, f := range []purge.Filter{a, b} {
		state, err := svc.Check(ctx, f)
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		for _, tb := range state {
			if tb.Deleting || tb.HasRows != tb.Rebuilt {
				t.Errorf("after the deletes: %+v", tb)
			}
		}
	}
	for table, want := range keptRows {
		if got := rowCount(t, ch, table, kept); got != want {
			t.Errorf("%s: kept project has %d rows, want %d", table, got, want)
		}
	}
}

func TestStartRefusesWithoutADeletedProject(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	ch := testutil.SetupClickHouse(t)
	ctx := t.Context()
	svc := purge.NewService(pg.PgW, ch.Conn)

	if _, err := svc.Start(ctx, purge.Filter{}); !errors.Is(err, purge.ErrNoProject) {
		t.Errorf("Start with no project = %v, want ErrNoProject", err)
	}
	if _, err := svc.Check(ctx, purge.Filter{}); !errors.Is(err, purge.ErrNoProject) {
		t.Errorf("Check with no project = %v, want ErrNoProject", err)
	}

	w := dbwrite.New(pg.PgW)
	orgID := xid.New().String()
	if _, err := w.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: orgID, DisplayName: "Org"}); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	project, err := projects.CreateProjectInTx(ctx, w, orgID, "Live", "")
	if err != nil {
		t.Fatalf("CreateProjectInTx: %v", err)
	}
	seedClickHouse(t, ch, project.ID, time.Now().UTC())
	live := purge.Filter{ProjectID: project.ID}

	if _, err := svc.Start(ctx, live); !errors.Is(err, purge.ErrLiveProject) {
		t.Errorf("Start on a live project = %v, want ErrLiveProject", err)
	}
	if queued := queuedDeletes(t, ch, project.ID); len(queued) != 0 {
		t.Errorf("deletes queued for a live project: %v", queued)
	}

	if err := w.HideProject(ctx, project.ID); err != nil {
		t.Fatalf("HideProject: %v", err)
	}
	if queued, err := svc.Start(ctx, live); err != nil || len(queued) == 0 {
		t.Errorf("Start on a hidden project = %v, %v; want its deletes queued", queued, err)
	}
}
