package mcpgrants

import (
	"errors"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/pug-sh/pug/internal/cimd"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
)

const claudeCodeID = "https://claude.ai/oauth/claude-code-client-metadata"

var claudeCode = fetchFunc(func(clientID string) (cimd.Document, error) {
	if clientID != claudeCodeID {
		return claude(clientID)
	}
	return cimd.Document{ClientID: claudeCodeID, ClientName: "Claude Code", RedirectURIs: []string{"http://localhost/callback"}}, nil
})

// another signs up a second person in the same database.
func (f *fixture) another(email string) *fixture {
	f.t.Helper()
	g := *f
	g.customer, g.orgID, g.project = f.signUp(email)
	return &g
}

func (f *fixture) join(orgID string) {
	f.t.Helper()
	if _, err := dbwrite.New(f.db.PgW).CreateOrgMember(f.t.Context(), dbwrite.CreateOrgMemberParams{
		CustomerID: f.customer, OrgID: orgID, Role: "ORG_ROLE_VIEWER",
	}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) grants() []Grant {
	f.t.Helper()
	grants, err := f.svc.ListGrants(f.t.Context(), f.customer)
	if err != nil {
		f.t.Fatalf("ListGrants: %v", err)
	}
	return grants
}

func (f *fixture) grantID(tokens Tokens) string {
	f.t.Helper()
	access, err := f.svc.ResolveAccessToken(f.t.Context(), tokens.AccessToken)
	if err != nil {
		f.t.Fatalf("ResolveAccessToken: %v", err)
	}
	return access.GrantID
}

func TestDescribe(t *testing.T) {
	f := newFixture(t, "bob@acme.com")
	carol := f.another("carol@globex.com")
	f.join(carol.orgID)
	dave := f.another("dave@initech.com")
	f.join(dave.orgID)
	f.exec("update projects set deletion_time = now() where id = $1", dave.project)
	f.another("erin@umbrella.com")

	c, err := f.svc.Describe(t.Context(), f.customer, request(claudeID, claudeRedirect))
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if c.ClientHost != "claude.ai" || c.ClientName != "Claude" || c.RedirectHost != "claude.ai" || c.Loopback {
		t.Errorf("consent = %+v, want claude.ai's, off loopback", c)
	}
	var ids []string
	for _, p := range c.Projects {
		ids = append(ids, p.ID)
	}
	if want := []string{f.project, carol.project}; !slices.Equal(ids, want) {
		t.Fatalf("projects = %v, want %v: the person's live ones, any role, oldest org first", ids, want)
	}
	if p := c.Projects[1]; p.OrgID != carol.orgID || p.OrgDisplayName != "Org of carol@globex.com" || p.DisplayName == "" {
		t.Errorf("project = %+v, want carol's, with its org's name", p)
	}
	cancel, err := url.Parse(c.CancelURL)
	if err != nil {
		t.Fatal(err)
	}
	q := cancel.Query()
	if cancel.Scheme+"://"+cancel.Host+cancel.Path != claudeRedirect || q.Get("error") != "access_denied" ||
		q.Get("state") != "state-1" || q.Get("iss") != testIssuer || q.Has("code") {
		t.Errorf("cancel = %s, want the redirect with access_denied, the state and iss", c.CancelURL)
	}

	if _, err := f.svc.Describe(t.Context(), f.customer, request(claudeID, "https://claude.ai/other")); !errors.Is(err, ErrRedirectNotListed) {
		t.Errorf("an unlisted redirect: err = %v, want ErrRedirectNotListed", err)
	}
}

func TestDescribeLoopback(t *testing.T) {
	f := newFixture(t, "bob@acme.com")
	f.svc = NewService(f.db.PgW, claudeCode, testIssuer)
	c, err := f.svc.Describe(t.Context(), f.customer, request(claudeCodeID, "http://localhost:53682/callback"))
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if !c.Loopback || c.ClientHost != "claude.ai" || c.RedirectHost != "localhost:53682" {
		t.Errorf("consent = %+v, want a loopback redirect to localhost:53682", c)
	}
}

func TestListGrants(t *testing.T) {
	f := newFixture(t, "bob@acme.com")
	session := f.signIn(f.customer, "")
	older := f.connect(session)
	newer := f.connect(session)
	// Approved, never exchanged.
	f.approve(session)
	carol := f.another("carol@globex.com")
	carol.connect(carol.signIn(carol.customer, ""))
	f.exec("update mcp_grants set last_used_time = create_time + interval '1 minute'")

	grants := f.grants()
	var ids []string
	for _, g := range grants {
		ids = append(ids, g.ID)
	}
	if want := []string{f.grantID(newer), f.grantID(older)}; !slices.Equal(ids, want) {
		t.Fatalf("grants = %v, want %v: the two exchanged ones, newest first", ids, want)
	}
	g := grants[0]
	if g.ClientHost != "claude.ai" || g.ClientName != "Claude" || !slices.Equal(g.Orgs, []Org{{f.orgID, "Org of bob@acme.com"}}) ||
		g.CreateTime.IsZero() || g.LastUsedTime.Sub(g.CreateTime) != time.Minute {
		t.Errorf("grant = %+v, want claude.ai on bob's org, used a minute after it was created", g)
	}

	// A refreshed grant has two refresh rows, and an idle one no live access token.
	if _, err := f.refresh(older.RefreshToken); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	f.exec("update refresh_tokens set access_expires_at = now() where mcp_grant_id is not null")
	if n := len(f.grants()); n != 2 {
		t.Fatalf("%d grants once refreshed and idle, want the same 2", n)
	}
}

func TestListGrantsHidesDeadGrants(t *testing.T) {
	for name, kill := range map[string]func(*fixture, Tokens){
		"revoked": func(f *fixture, tokens Tokens) {
			f.exec("update mcp_grants set revoke_time = now() where id = $1", f.grantID(tokens))
		},
		"left the org and was added back": func(f *fixture, _ Tokens) {
			f.exec("delete from org_members where customer_id = $1", f.customer)
			f.join(f.orgID)
		},
		"refresh token expired": func(f *fixture, tokens Tokens) {
			f.exec("update refresh_tokens set expires_at = now() where token_hash = $1", hashToken(tokens.RefreshToken))
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, "bob@acme.com")
			tokens := f.connect(f.signIn(f.customer, ""))
			if n := len(f.grants()); n != 1 {
				t.Fatalf("%d grants before, want 1", n)
			}
			kill(f, tokens)
			if grants := f.grants(); len(grants) != 0 {
				t.Errorf("grants = %+v, want none", grants)
			}
		})
	}
}

// A grant names the orgs it reads: those its person joined before approving and
// hasn't left since.
func TestListGrantsNamesItsOrgs(t *testing.T) {
	f := newFixture(t, "bob@acme.com")
	carol := f.another("carol@globex.com")
	f.join(carol.orgID)
	dave := f.another("dave@initech.com")
	f.connect(f.signIn(f.customer, ""))
	f.join(dave.orgID)

	bobs := Org{f.orgID, "Org of bob@acme.com"}
	if grants := f.grants(); len(grants) != 1 || !slices.Equal(grants[0].Orgs, []Org{bobs, {carol.orgID, "Org of carol@globex.com"}}) {
		t.Fatalf("grants = %+v, want one reading bob's and carol's orgs, not the one joined since", grants)
	}
	f.exec("delete from org_members where customer_id = $1 and org_id = $2", f.customer, carol.orgID)
	if grants := f.grants(); len(grants) != 1 || !slices.Equal(grants[0].Orgs, []Org{bobs}) {
		t.Errorf("grants = %+v, want one reading bob's org alone", grants)
	}
}

func TestRevokeGrant(t *testing.T) {
	f := newFixture(t, "bob@acme.com")
	carol := f.another("carol@globex.com")
	tokens := f.connect(f.signIn(f.customer, ""))
	id := f.grantID(tokens)

	for _, tc := range []struct{ name, customer, grant string }{
		{"another person's grant", carol.customer, id},
		{"an unknown grant", f.customer, "d0000000000000000000"},
	} {
		if err := f.svc.RevokeGrant(t.Context(), tc.customer, tc.grant); !errors.Is(err, ErrGrantNotFound) {
			t.Errorf("%s: err = %v, want ErrGrantNotFound", tc.name, err)
		}
	}
	if f.revoked(tokens) {
		t.Fatal("another person revoked the grant")
	}
	if err := f.svc.RevokeGrant(t.Context(), f.customer, id); err != nil {
		t.Fatalf("RevokeGrant: %v", err)
	}
	f.wantDead(tokens)
	if err := f.svc.RevokeGrant(t.Context(), f.customer, id); !errors.Is(err, ErrGrantNotFound) {
		t.Errorf("a second revoke: err = %v, want ErrGrantNotFound", err)
	}
}
