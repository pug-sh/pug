package projects

import (
	"context"
	"slices"
	"testing"
	"time"

	"connectrpc.com/authn"
	"connectrpc.com/connect"
	"github.com/rs/xid"

	"github.com/pug-sh/pug/internal/app/server/rpc"
	coremcpgrants "github.com/pug-sh/pug/internal/core/mcpgrants"
	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	"github.com/pug-sh/pug/internal/deps/postgres"
	sharedprojectsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/projects/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
)

// Bob is in his own org and carol's, which he joined before connecting an app,
// and dave's, which he joined after. Erin's org, which he never joins, and carol's
// second project catch a list wider than the credential.
func TestList(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	ctx := t.Context()
	r, w := dbread.New(db.PgW), dbwrite.New(db.PgW)
	orgs := coreorgs.NewService(db.PgRO, db.PgW, nil)
	orgNames := map[string]string{}

	signUp := func(email string) (dbread.Customer, dbread.Project) {
		t.Helper()
		id := xid.New().String()
		if _, err := w.CreateCustomer(ctx, dbwrite.CreateCustomerParams{ID: id, Email: email}); err != nil {
			t.Fatal(err)
		}
		customer, err := r.GetCustomerByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		org, err := orgs.CreateOrgWithDefaults(ctx, id, "Org of "+email)
		if err != nil {
			t.Fatal(err)
		}
		orgNames[org.ID] = org.DisplayName
		projects, err := r.GetProjectsByOrgID(ctx, org.ID)
		if err != nil || len(projects) != 1 {
			t.Fatalf("projects = %v, %v; want the default one", projects, err)
		}
		return customer, projects[0]
	}
	join := func(customer dbread.Customer, orgID string) {
		t.Helper()
		if _, err := w.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{CustomerID: customer.ID, OrgID: orgID, Role: "ORG_ROLE_VIEWER"}); err != nil {
			t.Fatal(err)
		}
	}

	bob, bobs := signUp("bob@acme.com")
	_, carols := signUp("carol@globex.com")
	signUp("erin@hooli.com")
	join(bob, carols.OrgID)
	grantID := xid.New().String()
	if err := w.CreateMCPGrant(ctx, dbwrite.CreateMCPGrantParams{
		ClientID: "https://claude.ai/oauth/mcp-oauth-client-metadata", ClientName: "Claude", CodeChallenge: "c",
		CodeExpiresAt: postgres.NewTimestamptz(time.Now().Add(time.Minute)), CodeHash: grantID,
		CustomerID: bob.ID, ID: grantID, RedirectUri: "https://claude.ai/api/mcp/auth_callback",
	}); err != nil {
		t.Fatal(err)
	}
	sibling := dbread.Project{ID: xid.New().String(), DisplayName: "Sibling", OrgID: carols.OrgID}
	if _, err := w.CreateProject(ctx, dbwrite.CreateProjectParams{ID: sibling.ID, DisplayName: sibling.DisplayName, OrgID: sibling.OrgID}); err != nil {
		t.Fatal(err)
	}
	_, daves := signUp("dave@initech.com")
	join(bob, daves.OrgID)

	srv := NewServer(r, coremcpgrants.NewService(db.PgW, nil, "https://api.example"))
	list := func(principal *rpc.Principal) []*sharedprojectsv1.Project {
		t.Helper()
		resp, err := srv.List(authn.SetInfo(context.Background(), principal), connect.NewRequest(&sharedprojectsv1.ListRequest{}))
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		return resp.Msg.GetProjects()
	}
	listed := func(principal *rpc.Principal) []string {
		var out []string
		for _, p := range list(principal) {
			out = append(out, p.GetId()+" "+p.GetDisplayName()+" "+p.GetOrgId()+" "+p.GetOrgDisplayName())
		}
		return out
	}
	rows := func(projects ...dbread.Project) []string {
		var out []string
		for _, p := range projects {
			out = append(out, p.ID+" "+p.DisplayName+" "+p.OrgID+" "+orgNames[p.OrgID])
		}
		return out
	}

	if got, want := listed(&rpc.Principal{AuthType: rpc.AuthTypePrivateKey, Project: &carols}), rows(carols); !slices.Equal(got, want) {
		t.Errorf("an API key = %v, want %v: its own project, not its org's", got, want)
	}
	if got, want := listed(&rpc.Principal{AuthType: rpc.AuthTypeJWT, Customer: &bob}), rows(bobs, carols, sibling, daves); !slices.Equal(got, want) {
		t.Errorf("a dashboard session = %v, want %v: every org's, oldest first", got, want)
	}
	token := &rpc.Principal{AuthType: rpc.AuthTypeMCPToken, Customer: &bob, GrantID: grantID}
	if got, want := listed(token), rows(bobs, carols, sibling); !slices.Equal(got, want) {
		t.Errorf("an MCP token = %v, want %v: not the org joined after the grant", got, want)
	}
}
