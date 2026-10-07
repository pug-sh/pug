package projects

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	coreprojects "github.com/pug-sh/pug/internal/core/projects"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
	"github.com/rs/xid"
)

func TestMain(m *testing.M) { testutil.Main(m) }

func TestDelete(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	rd := testutil.SetupRedis(t)
	t.Setenv("DATABASE_URL", pg.PgW.Config().ConnString())
	t.Setenv("REDIS_URL", rd.URL)
	ctx := t.Context()

	orgID := xid.New().String()
	if _, err := dbwrite.New(pg.PgW).CreateOrg(ctx, dbwrite.CreateOrgParams{ID: orgID, DisplayName: "Org"}); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	repo := coreprojects.NewRepo(dbread.New(pg.PgRO), rd.Client)
	svc := coreprojects.NewService(pg.PgRO, pg.PgW, repo)
	live, err := svc.CreateProject(ctx, orgID, "Live", "")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	prv, err := svc.CreateApiKey(ctx, live.ID, coreprojects.KindPrivate, "")
	if err != nil {
		t.Fatalf("CreateApiKey: %v", err)
	}
	// Cached, so only the command's invalidation stops it resolving.
	if _, err := repo.GetProjectByPrivateApiKey(ctx, prv.RawKey); err != nil {
		t.Fatalf("resolve key: %v", err)
	}

	cli, err := New(ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { cli.Close(ctx) })
	run := func(t *testing.T, projectID, actor, want string) {
		t.Helper()
		var out strings.Builder
		if err := cli.Delete(ctx, &out, projectID, actor); err != nil {
			t.Fatalf("Delete(%s): %v", projectID, err)
		}
		if !strings.Contains(out.String(), want) {
			t.Fatalf("Delete(%s) = %q, want %q", projectID, out.String(), want)
		}
	}

	t.Run("a live project is hidden and queued", func(t *testing.T) {
		run(t, live.ID, "ops/1", "hidden")
		if got, want := deletion(t, pg, live.ID), (row{status: "pending", orgID: orgID, name: "Live", requestedBy: "ops/1"}); got != want {
			t.Errorf("deletion = %+v, want %+v", got, want)
		}
		var hidden bool
		if err := pg.PgW.QueryRow(ctx, "select deletion_time is not null from projects where id = $1", live.ID).Scan(&hidden); err != nil || !hidden {
			t.Errorf("hidden = %v, %v; want true", hidden, err)
		}
		if _, err := repo.GetProjectByPrivateApiKey(ctx, prv.RawKey); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("revoked key: err = %v, want ErrNoRows", err)
		}
		run(t, live.ID, "ops/2", "already open")
		if got := deletion(t, pg, live.ID); got.requestedBy != "ops/1" {
			t.Errorf("requested_by = %q after a second run, want ops/1", got.requestedBy)
		}
	})

	orphan := xid.New().String()
	t.Run("an id with no projects row is only queued", func(t *testing.T) {
		run(t, orphan, "ops/1", "no projects row")
		if got, want := deletion(t, pg, orphan), (row{status: "pending", requestedBy: "ops/1"}); got != want {
			t.Errorf("deletion = %+v, want %+v", got, want)
		}
		run(t, orphan, "ops/2", "already open")
	})

	t.Run("a done deletion is reopened", func(t *testing.T) {
		if _, err := pg.PgW.Exec(ctx,
			"update project_deletions set status = 'done', done_at = now() where project_id = $1", orphan); err != nil {
			t.Fatalf("finish the deletion: %v", err)
		}
		run(t, orphan, "ops/3", "reopened")
		if got := deletion(t, pg, orphan); got.status != "deleting" {
			t.Errorf("status = %q, want deleting", got.status)
		}
	})

	t.Run("a malformed id or a blank actor is refused", func(t *testing.T) {
		var out strings.Builder
		if err := cli.Delete(ctx, &out, "abc", "ops/1"); err == nil {
			t.Fatal("Delete(abc) succeeded")
		}
		if err := cli.Delete(ctx, &out, xid.New().String(), " "); err == nil {
			t.Fatal("Delete with a blank actor succeeded")
		}
		var n int
		if err := pg.PgW.QueryRow(ctx, "select count(*) from project_deletions").Scan(&n); err != nil || n != 2 {
			t.Errorf("deletions = %d, %v; want the 2 above", n, err)
		}
	})
}

type row struct {
	status, orgID, name, requestedBy string
}

func deletion(t *testing.T, pg *testutil.TestPostgres, projectID string) row {
	t.Helper()
	var r row
	if err := pg.PgW.QueryRow(t.Context(),
		"select status, coalesce(org_id, ''), display_name, requested_by from project_deletions where project_id = $1",
		projectID).Scan(&r.status, &r.orgID, &r.name, &r.requestedBy); err != nil {
		t.Fatalf("read deletion: %v", err)
	}
	return r
}
