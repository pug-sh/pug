package mcpgrants

import (
	"errors"
	"slices"
	"testing"

	"github.com/rs/xid"

	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
)

func (f *fixture) addProject(orgID string) string {
	f.t.Helper()
	p, err := dbwrite.New(f.db.PgW).CreateProject(f.t.Context(), dbwrite.CreateProjectParams{
		DisplayName: "Later", ID: xid.New().String(), OrgID: orgID, ReportingTimezone: "UTC",
	})
	if err != nil {
		f.t.Fatalf("CreateProject: %v", err)
	}
	return p.ID
}

func (f *fixture) listed(grantID string) []string {
	f.t.Helper()
	projects, err := f.svc.ListProjects(f.t.Context(), grantID)
	if err != nil {
		f.t.Fatalf("ListProjects: %v", err)
	}
	ids := make([]string, len(projects))
	for i, p := range projects {
		ids[i] = p.ID
	}
	return ids
}

// A grant reads every project in the orgs its person belonged to when they
// approved, projects added there later included, and nothing else.
func TestGrantReadsItsPersonsOrgs(t *testing.T) {
	f := newFixture(t, "bob@acme.com")
	carol := f.another("carol@globex.com")
	f.join(carol.orgID)
	dave := f.another("dave@initech.com")
	grant := f.grantID(f.connect(f.signIn(f.customer, "")))
	later := f.addProject(f.orgID)
	f.join(dave.orgID)

	if got, want := f.listed(grant), []string{f.project, later, carol.project}; !slices.Equal(got, want) {
		t.Fatalf("projects = %v, want %v: both orgs' live projects, oldest org first", got, want)
	}
	for _, tc := range []struct {
		name    string
		project string
		want    error
	}{
		{"the person's own org", f.project, nil},
		{"a project added after the grant", later, nil},
		{"an org joined before the grant", carol.project, nil},
		{"an org joined after the grant", dave.project, ErrProjectNotFound},
		{"an unknown project", "d0000000000000000000", ErrProjectNotFound},
	} {
		p, err := f.svc.ResolveProject(t.Context(), grant, tc.project)
		if !errors.Is(err, tc.want) || (err == nil && p.ID != tc.project) {
			t.Errorf("%s: ResolveProject = %q, %v; want %v", tc.name, p.ID, err, tc.want)
		}
	}
}

// Leaving one org, or deleting a project, takes away only that.
func TestGrantLosesOneOrg(t *testing.T) {
	f := newFixture(t, "bob@acme.com")
	carol := f.another("carol@globex.com")
	f.join(carol.orgID)
	tokens := f.connect(f.signIn(f.customer, ""))
	grant := f.grantID(tokens)

	f.exec("delete from org_members where customer_id = $1 and org_id = $2", f.customer, carol.orgID)
	f.exec("update projects set deletion_time = now() where id = $1", f.project)

	if got := f.listed(grant); len(got) != 0 {
		t.Errorf("projects = %v, want none: one org left, the other's only project deleted", got)
	}
	for _, id := range []string{f.project, carol.project} {
		if _, err := f.svc.ResolveProject(t.Context(), grant, id); !errors.Is(err, ErrProjectNotFound) {
			t.Errorf("ResolveProject(%s) err = %v, want ErrProjectNotFound", id, err)
		}
	}
	// Still in bob's org, where a new project would be read.
	if _, err := f.svc.ResolveAccessToken(t.Context(), tokens.AccessToken); err != nil {
		t.Errorf("ResolveAccessToken: %v, want the grant alive while it reads an org", err)
	}
	if _, err := f.refresh(tokens.RefreshToken); err != nil {
		t.Errorf("Refresh: %v, want the grant alive while it reads an org", err)
	}
}
