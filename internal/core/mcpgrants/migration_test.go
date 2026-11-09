package mcpgrants

import (
	"testing"

	"github.com/pug-sh/pug/internal/testutil"
)

// 030 revokes the grants approved for one project, and its Down deletes the ones
// that name none, with their tokens.
func TestMigration030(t *testing.T) {
	f := newFixture(t, "bob@acme.com")
	legacy := f.connect(f.signIn(f.customer, ""))
	f.exec("update mcp_grants set project_id = $1 where id = $2", f.project, f.grantID(legacy))
	current := f.grantID(f.connect(f.signIn(f.customer, "")))

	migrations := testutil.PostgresMigrations(t, f.db)
	if _, err := migrations.DownTo(t.Context(), 29); err != nil {
		t.Fatalf("migrate down to 029: %v", err)
	}
	var left int
	if err := f.db.PgW.QueryRow(t.Context(), `select (select count(*) from mcp_grants where id = $1)
		+ (select count(*) from refresh_tokens where mcp_grant_id = $1)`, current).Scan(&left); err != nil || left != 0 {
		t.Fatalf("rows left of the grant naming no project = %d, %v; want none", left, err)
	}
	if _, err := migrations.UpByOne(t.Context()); err != nil {
		t.Fatalf("migrate up to 030: %v", err)
	}
	f.wantDead(legacy)
}
