package mcpgrants

import (
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rs/xid"
	"golang.org/x/oauth2"

	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
)

type fixture struct {
	t        *testing.T
	db       *testutil.TestPostgres
	svc      *Service
	orgs     *coreorgs.Service
	orgID    string
	customer string
	project  string
}

// newFixture signs up email with an org and its default project.
func newFixture(t *testing.T, email string) *fixture {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	f := &fixture{
		t:    t,
		db:   db,
		svc:  NewService(db.PgW, claude, testIssuer),
		orgs: coreorgs.NewService(db.PgRO, db.PgW, nil),
	}
	f.customer, f.orgID, f.project = f.signUp(email)
	return f
}

func (f *fixture) signUp(email string) (customerID, orgID, projectID string) {
	f.t.Helper()
	ctx := f.t.Context()
	customerID = xid.New().String()
	if _, err := dbwrite.New(f.db.PgW).CreateCustomer(ctx, dbwrite.CreateCustomerParams{ID: customerID, Email: email}); err != nil {
		f.t.Fatalf("CreateCustomer: %v", err)
	}
	org, err := f.orgs.CreateOrgWithDefaults(ctx, customerID, "Org of "+email)
	if err != nil {
		f.t.Fatalf("CreateOrgWithDefaults: %v", err)
	}
	projects, err := dbread.New(f.db.PgW).GetProjectsByOrgID(ctx, org.ID)
	if err != nil || len(projects) != 1 {
		f.t.Fatalf("projects = %v, %v; want the default one", projects, err)
	}
	return customerID, org.ID, projects[0].ID
}

// signIn starts a dashboard session for customerID whose sign-in proved
// provenDomain, and returns its sid.
func (f *fixture) signIn(customerID, provenDomain string) string {
	f.t.Helper()
	family := xid.New().String()
	if _, err := dbwrite.New(f.db.PgW).CreateRefreshToken(f.t.Context(), dbwrite.CreateRefreshTokenParams{
		ID:           xid.New().String(),
		CustomerID:   customerID,
		FamilyID:     family,
		TokenHash:    hashToken(family),
		ExpiresAt:    postgres.NewTimestamptz(time.Now().Add(time.Hour)),
		ProvenDomain: postgres.NewOptionalText(provenDomain),
	}); err != nil {
		f.t.Fatalf("CreateRefreshToken: %v", err)
	}
	return family
}

func (f *fixture) approval(session, verifier string) Approval {
	req := request(claudeID, claudeRedirect)
	req.CodeChallenge = oauth2.S256ChallengeFromVerifier(verifier)
	return Approval{Request: req, CustomerID: f.customer, SessionID: session}
}

// approve allows claude.ai and returns the redirect it gets, with the PKCE verifier.
func (f *fixture) approve(session string) (url.Values, string) {
	f.t.Helper()
	verifier := oauth2.GenerateVerifier()
	redirect, err := f.svc.Approve(f.t.Context(), f.approval(session, verifier))
	if err != nil {
		f.t.Fatalf("Approve: %v", err)
	}
	u, err := url.Parse(redirect)
	if err != nil || u.Scheme+"://"+u.Host+u.Path != claudeRedirect {
		f.t.Fatalf("redirect = %s, %v; want %s", redirect, err, claudeRedirect)
	}
	return u.Query(), verifier
}

func (f *fixture) exchange(code, verifier string) (Tokens, error) {
	return f.svc.ExchangeCode(f.t.Context(), CodeExchange{
		ClientID: claudeID, Code: code, CodeVerifier: verifier, RedirectURI: claudeRedirect, Resource: testIssuer + "/mcp",
	})
}

// connect approves and exchanges the code, as a client's first connect does.
func (f *fixture) connect(session string) Tokens {
	f.t.Helper()
	q, verifier := f.approve(session)
	tokens, err := f.exchange(q.Get("code"), verifier)
	if err != nil {
		f.t.Fatalf("ExchangeCode: %v", err)
	}
	return tokens
}

func (f *fixture) refresh(refreshToken string) (Tokens, error) {
	return f.svc.Refresh(f.t.Context(), TokenRefresh{ClientID: claudeID, RefreshToken: refreshToken, Resource: testIssuer + "/mcp"})
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.db.PgW.Exec(f.t.Context(), sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

// revoked reports whether the grant that issued tokens is revoked.
func (f *fixture) revoked(tokens Tokens) bool {
	f.t.Helper()
	var revoked bool
	if err := f.db.PgW.QueryRow(f.t.Context(), `select g.revoke_time is not null from mcp_grants g
		join refresh_tokens rt on rt.mcp_grant_id = g.id where rt.token_hash = $1`,
		hashToken(tokens.RefreshToken)).Scan(&revoked); err != nil {
		f.t.Fatalf("read grant: %v", err)
	}
	return revoked
}

func (f *fixture) wantDead(tokens Tokens) {
	f.t.Helper()
	if _, err := f.svc.ResolveAccessToken(f.t.Context(), tokens.AccessToken); !errors.Is(err, ErrInvalidToken) {
		f.t.Errorf("ResolveAccessToken err = %v, want ErrInvalidToken", err)
	}
	if _, err := f.refresh(tokens.RefreshToken); !errors.Is(err, ErrInvalidGrant) {
		f.t.Errorf("Refresh err = %v, want ErrInvalidGrant", err)
	}
	if !f.revoked(tokens) {
		f.t.Error("the grant is not revoked")
	}
}

func TestConnectAndRefresh(t *testing.T) {
	f := newFixture(t, "bob@acme.com")
	ctx := t.Context()

	q, verifier := f.approve(f.signIn(f.customer, ""))
	if q.Get("code") == "" || q.Get("state") != "state-1" || q.Get("iss") != testIssuer {
		t.Fatalf("redirect query = %v, want a code, the state and iss", q)
	}
	// A failed exchange leaves the code for the right one.
	for _, tc := range []struct {
		name   string
		modify func(*CodeExchange)
		want   error
	}{
		{"another verifier", func(x *CodeExchange) { x.CodeVerifier = oauth2.GenerateVerifier() }, ErrInvalidGrant},
		{"another client", func(x *CodeExchange) { x.ClientID = chatGPTID }, ErrInvalidGrant},
		{"another redirect", func(x *CodeExchange) { x.RedirectURI = "https://claude.ai/api/other" }, ErrInvalidGrant},
		{"another resource", func(x *CodeExchange) { x.Resource = "https://evil.example/mcp" }, ErrInvalidTarget},
		{"no verifier", func(x *CodeExchange) { x.CodeVerifier = "" }, ErrInvalidRequest},
		{"an unknown code", func(x *CodeExchange) { x.Code = strings.Repeat("0", 64) }, ErrInvalidGrant},
	} {
		x := CodeExchange{ClientID: claudeID, Code: q.Get("code"), CodeVerifier: verifier, RedirectURI: claudeRedirect}
		tc.modify(&x)
		if _, err := f.svc.ExchangeCode(ctx, x); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	tokens, err := f.exchange(q.Get("code"), verifier)
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if !strings.HasPrefix(tokens.AccessToken, AccessTokenPrefix) || !strings.HasPrefix(tokens.RefreshToken, refreshTokenPrefix) {
		t.Fatalf("tokens = %+v, want mcp_at_ and mcp_rt_ prefixes", tokens)
	}

	access, err := f.svc.ResolveAccessToken(ctx, tokens.AccessToken)
	if err != nil || access.Customer.ID != f.customer || access.GrantID == "" {
		t.Fatalf("ResolveAccessToken = %+v, %v; want the person who approved", access, err)
	}
	if project, err := f.svc.ResolveProject(ctx, access.GrantID, f.project); err != nil || project.ID != f.project {
		t.Fatalf("ResolveProject = %+v, %v; want the person's project", project, err)
	}
	if _, err := f.svc.ResolveAccessToken(ctx, tokens.RefreshToken); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("a refresh token resolved as an access token: %v", err)
	}

	if _, err := f.svc.Refresh(ctx, TokenRefresh{ClientID: chatGPTID, RefreshToken: tokens.RefreshToken}); !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("refresh by another client err = %v, want ErrInvalidGrant", err)
	}
	next, err := f.refresh(tokens.RefreshToken)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	// Calls in flight keep working after a rotation.
	for _, at := range []string{tokens.AccessToken, next.AccessToken} {
		if _, err := f.svc.ResolveAccessToken(ctx, at); err != nil {
			t.Fatalf("ResolveAccessToken after rotation: %v", err)
		}
	}
	f.exec("update refresh_tokens set access_expires_at = now() where access_token_hash = $1", hashToken(next.AccessToken))
	if _, err := f.svc.ResolveAccessToken(ctx, next.AccessToken); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("an expired access token resolved: %v", err)
	}
}

// The challenge matches each verifier, so only RFC 7636's shape refuses one.
func TestVerifierShape(t *testing.T) {
	f := newFixture(t, "bob@acme.com")
	session := f.signIn(f.customer, "")
	for _, tc := range []struct {
		verifier string
		valid    bool
	}{
		{strings.Repeat("a", 43), true},
		{strings.Repeat("a", 128), true},
		{strings.Repeat("a", 42), false},
		{strings.Repeat("a", 129), false},
		{strings.Repeat("a", 42) + "+", false},
	} {
		redirect, err := f.svc.Approve(t.Context(), f.approval(session, tc.verifier))
		if err != nil {
			t.Fatalf("Approve: %v", err)
		}
		u, err := url.Parse(redirect)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.exchange(u.Query().Get("code"), tc.verifier)
		if tc.valid && err != nil || !tc.valid && !errors.Is(err, ErrInvalidGrant) {
			t.Errorf("verifier of %d: err = %v, want valid = %v", len(tc.verifier), err, tc.valid)
		}
	}
}

func TestExpiredCodeIsRefused(t *testing.T) {
	f := newFixture(t, "bob@acme.com")
	q, verifier := f.approve(f.signIn(f.customer, ""))
	f.exec("update mcp_grants set code_expires_at = now()")
	if _, err := f.exchange(q.Get("code"), verifier); !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("err = %v, want ErrInvalidGrant", err)
	}
}

func TestExpiredRefreshTokenIsRefused(t *testing.T) {
	f := newFixture(t, "bob@acme.com")
	tokens := f.connect(f.signIn(f.customer, ""))
	f.exec("update refresh_tokens set expires_at = now() - interval '1 minute' where token_hash = $1", hashToken(tokens.RefreshToken))
	if _, err := f.refresh(tokens.RefreshToken); !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("err = %v, want ErrInvalidGrant", err)
	}
}

func TestReplayedCodeRevokesItsGrant(t *testing.T) {
	f := newFixture(t, "bob@acme.com")
	q, verifier := f.approve(f.signIn(f.customer, ""))
	tokens, err := f.exchange(q.Get("code"), verifier)
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if _, err := f.exchange(q.Get("code"), verifier); !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("replay err = %v, want ErrInvalidGrant", err)
	}
	f.wantDead(tokens)
}

func TestRefreshTokenReuse(t *testing.T) {
	f := newFixture(t, "bob@acme.com")
	tokens := f.connect(f.signIn(f.customer, ""))
	next, err := f.refresh(tokens.RefreshToken)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	// Moves the rotation back in time, and leaves an unused token unused.
	backdate := func(by time.Duration) {
		f.exec("update refresh_tokens set consumed_at = consumed_at - make_interval(secs => $2) where token_hash = $1",
			hashToken(tokens.RefreshToken), by.Seconds())
	}

	// A retry of a rotation whose answer was lost.
	backdate(reuseGrace - 5*time.Second)
	retry, err := f.refresh(tokens.RefreshToken)
	if err != nil {
		t.Fatalf("reuse within the grace: %v", err)
	}
	for _, pair := range []Tokens{next, retry} {
		if _, err := f.svc.ResolveAccessToken(t.Context(), pair.AccessToken); err != nil {
			t.Fatalf("a pair from the rotation stopped working: %v", err)
		}
	}

	backdate(6 * time.Second)
	if _, err := f.refresh(tokens.RefreshToken); !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("reuse after the grace err = %v, want ErrInvalidGrant", err)
	}
	f.wantDead(next)
	f.wantDead(retry)
}

// Clients can't be made to single-flight a refresh, so one racing a rotation of
// the same token waits for it and then takes the grace.
func TestRefreshRacingARotation(t *testing.T) {
	f := newFixture(t, "bob@acme.com")
	ctx := t.Context()
	tokens := f.connect(f.signIn(f.customer, ""))

	rotation, err := f.db.PgW.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rotation.Rollback(ctx) }()
	if _, err := rotation.Exec(ctx, "update refresh_tokens set consumed_at = now() where token_hash = $1",
		hashToken(tokens.RefreshToken)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := f.refresh(tokens.RefreshToken)
		done <- err
	}()
	for waiting := 0; waiting == 0; {
		select {
		case err := <-done:
			t.Fatalf("the refresh returned while the rotation held its token: %v", err)
		default:
		}
		if err := f.db.PgW.QueryRow(ctx, `select count(*) from pg_stat_activity
			where datname = current_database() and wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
	}
	if err := rotation.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("the racing refresh failed: %v", err)
	}
}

func TestRequireSSOAtApprovalAndRefresh(t *testing.T) {
	f := newFixture(t, "bob@acme.com")
	ctx := t.Context()
	byPassword := f.connect(f.signIn(f.customer, ""))
	bySSO := f.connect(f.signIn(f.customer, "acme.com"))

	domain, err := f.orgs.VerifyDomainByOperator(ctx, f.orgID, "acme.com")
	if err != nil {
		t.Fatalf("VerifyDomainByOperator: %v", err)
	}
	if err := coreorgs.MarkSSOSeenInTx(ctx, dbwrite.New(f.db.PgW), "acme.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.orgs.UpdateDomain(ctx, f.orgID, domain.ID, true); err != nil {
		t.Fatalf("UpdateDomain: %v", err)
	}

	_, err = f.svc.Approve(ctx, f.approval(f.signIn(f.customer, ""), oauth2.GenerateVerifier()))
	if ssoErr, ok := errors.AsType[*coreorgs.SSORequiredError](err); !ok || ssoErr.Domain != "acme.com" {
		t.Fatalf("approval by password err = %v, want SSORequiredError", err)
	}
	f.connect(f.signIn(f.customer, "acme.com"))

	_, err = f.refresh(byPassword.RefreshToken)
	if _, ok := errors.AsType[*coreorgs.SSORequiredError](err); !ok || !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("refresh of a password grant err = %v, want ErrInvalidGrant from SSORequiredError", err)
	}
	f.wantDead(byPassword)
	if _, err := f.refresh(bySSO.RefreshToken); err != nil {
		t.Fatalf("refresh of an SSO grant: %v", err)
	}
}

func TestLostAccessRevokesAtRefresh(t *testing.T) {
	leave := func(f *fixture) {
		if _, err := dbwrite.New(f.db.PgW).DeleteOrgMember(f.t.Context(), dbwrite.DeleteOrgMemberParams{
			OrgID: f.orgID, CustomerID: f.customer,
		}); err != nil {
			f.t.Fatal(err)
		}
	}
	for name, lose := range map[string]func(*fixture){
		"left the org": leave,
		// Before the client refreshed, so nothing saw the gap.
		"left and was added back": func(f *fixture) {
			leave(f)
			if _, err := dbwrite.New(f.db.PgW).CreateOrgMember(f.t.Context(), dbwrite.CreateOrgMemberParams{
				CustomerID: f.customer, OrgID: f.orgID, Role: "ORG_ROLE_MEMBER",
			}); err != nil {
				f.t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, "bob@acme.com")
			tokens := f.connect(f.signIn(f.customer, ""))
			lose(f)
			f.wantDead(tokens)
		})
	}
}

func TestApproveRefusals(t *testing.T) {
	f := newFixture(t, "bob@acme.com")
	otherCustomer, _, _ := f.signUp("carol@globex.com")
	signedOut := f.signIn(f.customer, "")
	f.exec("update refresh_tokens set revoked_at = now() where family_id = $1", signedOut)
	expired := f.signIn(f.customer, "")
	f.exec("update refresh_tokens set expires_at = now() where family_id = $1", expired)
	connection, err := f.svc.ResolveAccessToken(t.Context(), f.connect(f.signIn(f.customer, "")).AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		session string
		want    error
	}{
		{"a JWT minted without a sid", "", ErrSessionEnded},
		{"a signed-out session", signedOut, ErrSessionEnded},
		{"an expired session", expired, ErrSessionEnded},
		{"another person's session", f.signIn(otherCustomer, ""), ErrSessionEnded},
		{"a connection's token family", connection.GrantID, ErrSessionEnded},
	} {
		a := f.approval(tc.session, oauth2.GenerateVerifier())
		if _, err := f.svc.Approve(t.Context(), a); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	orgless, _, _ := f.signUp("dave@initech.com")
	f.exec("delete from org_members where customer_id = $1", orgless)
	a := f.approval(f.signIn(orgless, ""), oauth2.GenerateVerifier())
	a.CustomerID = orgless
	if _, err := f.svc.Approve(t.Context(), a); !errors.Is(err, ErrNoOrg) {
		t.Errorf("a person in no org: err = %v, want %v", err, ErrNoOrg)
	}
	var grants int
	if err := f.db.PgW.QueryRow(t.Context(), "select count(*) from mcp_grants").Scan(&grants); err != nil || grants != 1 {
		t.Fatalf("grants = %d, %v; want only the connection's", grants, err)
	}
}
