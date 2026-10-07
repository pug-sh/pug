package auth_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/rs/xid"

	coreauth "github.com/pug-sh/pug/internal/core/auth"
	coreoauth "github.com/pug-sh/pug/internal/core/auth/oauth"
	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	"github.com/pug-sh/pug/internal/core/projects"
	orgsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/orgs/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
)

type ssoFixture struct {
	t    *testing.T
	ctx  context.Context
	db   *testutil.TestPostgres
	orgs *coreorgs.Service
	read *dbread.Queries
}

func newSSOFixture(t *testing.T) *ssoFixture {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	return &ssoFixture{
		t:    t,
		ctx:  context.Background(),
		db:   db,
		orgs: coreorgs.NewService(db.PgRO, db.PgW, &stubPublisher{}),
		read: dbread.New(db.PgW),
	}
}

// org creates an org with domain verified by the operator, so settings skip DNS.
func (f *ssoFixture) org(domain string, settings coreorgs.DomainSettings) string {
	f.t.Helper()
	adminID := xid.New().String()
	if _, err := dbwrite.New(f.db.PgW).CreateCustomer(f.ctx, dbwrite.CreateCustomerParams{
		ID: adminID, Email: "admin-" + adminID + "@example.com", DisplayName: "", PictureUri: "", PasswordHash: "x",
	}); err != nil {
		f.t.Fatalf("create admin: %v", err)
	}
	org, err := f.orgs.CreateOrgWithDefaults(f.ctx, adminID, "org for "+domain)
	if err != nil {
		f.t.Fatalf("create org: %v", err)
	}
	if _, err := f.orgs.VerifyDomainByOperator(f.ctx, org.ID, domain); err != nil {
		f.t.Fatalf("verify domain: %v", err)
	}
	if _, err := f.orgs.SetDomainSettings(f.ctx, org.ID, settings); err != nil {
		f.t.Fatalf("set domain settings: %v", err)
	}
	return org.ID
}

// ssoService signs in the given identity on every CompleteOIDCSignIn.
func (f *ssoFixture) ssoService(email, sub, provenDomain string) *coreauth.Service {
	f.t.Helper()
	identity, err := coreoauth.NewVerifiedIdentity(testOIDCProvider, coreoauth.Claims{
		Subject: sub, Email: email, EmailVerified: true, ProvenDomain: provenDomain,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return coreauth.NewServiceWithOAuthForTest(f.ctx, f.db.PgRO, f.db.PgW, []byte("test-secret-key-for-jwt"), &stubPublisher{},
		coreoauth.TestConfig("client-id"), coreoauth.NewRegistry(mockOAuthProvider{identity: identity}))
}

func (f *ssoFixture) signIn(svc *coreauth.Service) coreauth.Session {
	f.t.Helper()
	session, err := svc.CompleteOIDCSignIn(f.ctx, testOIDCProvider, coreoauth.AuthorizationCode{Code: "code"}, "", "")
	if err != nil {
		f.t.Fatalf("CompleteOIDCSignIn: %v", err)
	}
	return session
}

func (f *ssoFixture) orgsOf(email string) map[string]string {
	f.t.Helper()
	customer, err := f.read.GetCustomerByEmail(f.ctx, email)
	if err != nil {
		f.t.Fatalf("get customer %s: %v", email, err)
	}
	rows, err := f.read.GetOrgsWithRoleByCustomerID(f.ctx, customer.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	out := map[string]string{}
	for _, r := range rows {
		out[r.ID] = r.Role
	}
	return out
}

func (f *ssoFixture) provenDomainOf(refreshToken string) string {
	f.t.Helper()
	var d *string
	if err := f.db.PgW.QueryRow(f.ctx, "select proven_domain from refresh_tokens where token_hash = $1", hashToken(refreshToken)).Scan(&d); err != nil {
		f.t.Fatal(err)
	}
	if d == nil {
		return ""
	}
	return *d
}

func TestSSOSignInAutoJoinsWithoutADefaultOrg(t *testing.T) {
	f := newSSOFixture(t)
	orgID := f.org("acme.com", coreorgs.DomainSettings{AutoJoinRole: coreorgs.RoleViewer, MembersCanCreateOrgs: true})

	session := f.signIn(f.ssoService("bob@acme.com", "sub-bob", "acme.com"))
	if !slices.Equal(session.JoinedOrgIDs, []string{orgID}) {
		t.Fatalf("joined = %v, want %s", session.JoinedOrgIDs, orgID)
	}
	if got := f.orgsOf("bob@acme.com"); len(got) != 1 || got[orgID] != coreorgs.RoleViewer.String() {
		t.Fatalf("orgs = %v, want only %s as viewer (no default org)", got, orgID)
	}
	if d := f.provenDomainOf(session.RefreshToken); d != "acme.com" {
		t.Fatalf("session proven domain = %q, want acme.com", d)
	}

	// A returning user joins an org that turned auto-join on later.
	second := f.org("acme.com", coreorgs.DomainSettings{AutoJoinRole: coreorgs.RoleMember, MembersCanCreateOrgs: true})
	again := f.signIn(f.ssoService("bob@acme.com", "sub-bob", "acme.com"))
	if !slices.Equal(again.JoinedOrgIDs, []string{second}) {
		t.Fatalf("returning sign-in joined %v, want %s", again.JoinedOrgIDs, second)
	}
	if got := f.orgsOf("bob@acme.com"); got[second] != coreorgs.RoleMember.String() {
		t.Fatalf("orgs = %v, want %s as member", got, second)
	}
}

func TestSSOSignInWithAutoJoinOffGetsADefaultOrg(t *testing.T) {
	f := newSSOFixture(t)
	orgID := f.org("acme.com", coreorgs.DomainSettings{MembersCanCreateOrgs: true})

	session := f.signIn(f.ssoService("bob@acme.com", "sub-bob", "acme.com"))
	got := f.orgsOf("bob@acme.com")
	if len(session.JoinedOrgIDs) != 0 || len(got) != 1 || got[orgID] != "" {
		t.Fatalf("joined = %v, orgs = %v; want only a default org of their own", session.JoinedOrgIDs, got)
	}

	// A returning user who joins nothing gets no second default org.
	f.signIn(f.ssoService("bob@acme.com", "sub-bob", "acme.com"))
	if got := f.orgsOf("bob@acme.com"); len(got) != 1 {
		t.Fatalf("orgs after a second sign-in = %v, want still one", got)
	}
}

func TestPersonalAccountOnTheDomainDoesNotAutoJoin(t *testing.T) {
	f := newSSOFixture(t)
	orgID := f.org("acme.com", coreorgs.DomainSettings{AutoJoinRole: coreorgs.RoleViewer, MembersCanCreateOrgs: true})

	session := f.signIn(f.ssoService("bob@acme.com", "sub-personal", ""))
	if got := f.orgsOf("bob@acme.com"); len(session.JoinedOrgIDs) != 0 || got[orgID] != "" || len(got) != 1 {
		t.Fatalf("joined = %v, orgs = %v; want no auto-join and a default org", session.JoinedOrgIDs, got)
	}
	if d := f.provenDomainOf(session.RefreshToken); d != "" {
		t.Fatalf("session proven domain = %q, want none", d)
	}
	if claims, err := f.orgs.DomainClaims(f.ctx, "acme.com"); err != nil || len(claims) != 1 || claims[0].SsoSeenAt.Valid {
		t.Fatalf("claims = %+v, %v; want sso_seen_at unset by a personal account", claims, err)
	}
}

func TestNewAccountGetsNoOrgWhereCreationIsRestricted(t *testing.T) {
	f := newSSOFixture(t)
	f.org("acme.com", coreorgs.DomainSettings{MembersCanCreateOrgs: false})

	session := f.signIn(f.ssoService("bob@acme.com", "sub-bob", "acme.com"))
	if got := f.orgsOf("bob@acme.com"); len(session.JoinedOrgIDs) != 0 || len(got) != 0 {
		t.Fatalf("joined = %v, orgs = %v; want no org at all", session.JoinedOrgIDs, got)
	}

	// The restriction is a limit, so it holds for an email link too.
	pub := &stubPublisher{}
	svc := mustNewTestAuthService(t, f.db, pub)
	if err := svc.RequestMagicLink(f.ctx, "carol@acme.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CompleteMagicLink(f.ctx, lastMagicToken(t, pub), ""); err != nil {
		t.Fatalf("CompleteMagicLink: %v", err)
	}
	if got := f.orgsOf("carol@acme.com"); len(got) != 0 {
		t.Fatalf("orgs = %v, want none for a magic-link sign-up on a restricted domain", got)
	}
}

func TestMagicLinkNeverAutoJoins(t *testing.T) {
	f := newSSOFixture(t)
	orgID := f.org("acme.com", coreorgs.DomainSettings{AutoJoinRole: coreorgs.RoleMember, MembersCanCreateOrgs: true})

	pub := &stubPublisher{}
	svc := mustNewTestAuthService(t, f.db, pub)
	if err := svc.RequestMagicLink(f.ctx, "bob@acme.com"); err != nil {
		t.Fatal(err)
	}
	session, err := svc.CompleteMagicLink(f.ctx, lastMagicToken(t, pub), "")
	if err != nil {
		t.Fatalf("CompleteMagicLink: %v", err)
	}
	if got := f.orgsOf("bob@acme.com"); len(session.JoinedOrgIDs) != 0 || got[orgID] != "" || len(got) != 1 {
		t.Fatalf("joined = %v, orgs = %v; want a default org and no auto-join", session.JoinedOrgIDs, got)
	}
	if _, err := svc.RefreshSession(f.ctx, session.RefreshToken); err != nil {
		t.Fatal(err)
	}
	if got := f.orgsOf("bob@acme.com"); got[orgID] != "" {
		t.Fatal("an email-link session auto-joined at refresh")
	}
}

func TestRefreshAutoJoinsForTheSessionsProvenDomain(t *testing.T) {
	f := newSSOFixture(t)
	ssoSession := f.signIn(f.ssoService("bob@acme.com", "sub-bob", "acme.com"))

	passwordSvc := mustNewTestAuthService(t, f.db, &stubPublisher{})
	passwordSession := seedSignedInCustomer(t, passwordSvc, dbwrite.New(f.db.PgW), xid.New().String(), "carol@acme.com")

	first := f.org("acme.com", coreorgs.DomainSettings{AutoJoinRole: coreorgs.RoleViewer, MembersCanCreateOrgs: true})
	rotated, err := passwordSvc.RefreshSession(f.ctx, ssoSession.RefreshToken)
	if err != nil {
		t.Fatalf("RefreshSession: %v", err)
	}
	if got := f.orgsOf("bob@acme.com"); got[first] != coreorgs.RoleViewer.String() {
		t.Fatalf("orgs = %v, want %s joined at refresh", got, first)
	}
	if d := f.provenDomainOf(rotated.RefreshToken); d != "acme.com" {
		t.Fatalf("rotated token proven domain = %q, want it copied", d)
	}
	// A refresh carries old proof, so only a fresh SSO sign-in marks the claim.
	if claims, err := f.orgs.DomainClaims(f.ctx, "acme.com"); err != nil || len(claims) != 1 || claims[0].SsoSeenAt.Valid {
		t.Fatalf("claims = %+v, %v; want sso_seen_at unset after a refresh", claims, err)
	}

	second := f.org("acme.com", coreorgs.DomainSettings{AutoJoinRole: coreorgs.RoleMember, MembersCanCreateOrgs: true})
	if _, err := passwordSvc.RefreshSession(f.ctx, rotated.RefreshToken); err != nil {
		t.Fatalf("second RefreshSession: %v", err)
	}
	if got := f.orgsOf("bob@acme.com"); got[second] != coreorgs.RoleMember.String() {
		t.Fatalf("orgs = %v, want %s joined at the next refresh", got, second)
	}

	if _, err := passwordSvc.RefreshSession(f.ctx, passwordSession.RefreshToken); err != nil {
		t.Fatalf("password RefreshSession: %v", err)
	}
	if got := f.orgsOf("carol@acme.com"); got[first] != "" || got[second] != "" {
		t.Fatalf("a password session auto-joined at refresh: %v", got)
	}
}

func TestRefreshSucceedsWhenAutoJoinFails(t *testing.T) {
	f := newSSOFixture(t)
	session := f.signIn(f.ssoService("bob@acme.com", "sub-bob", "acme.com"))

	// Each test has its own database, so dropping this table is safe.
	if _, err := f.db.PgW.Exec(f.ctx, "drop table org_domains"); err != nil {
		t.Fatal(err)
	}
	svc := mustNewTestAuthService(t, f.db, &stubPublisher{})
	rotated, err := svc.RefreshSession(f.ctx, session.RefreshToken)
	if err != nil || rotated.RefreshToken == "" {
		t.Fatalf("RefreshSession = %+v, %v; want the refresh to succeed", rotated, err)
	}
	if _, err := svc.RefreshSession(f.ctx, session.RefreshToken); !errors.Is(err, coreauth.ErrInvalidToken) {
		t.Fatalf("replayed token err = %v, want the rotation committed", err)
	}
}

func TestConcurrentFirstSSOSignInsMakeOneAccount(t *testing.T) {
	f := newSSOFixture(t)
	orgID := f.org("acme.com", coreorgs.DomainSettings{AutoJoinRole: coreorgs.RoleMember, MembersCanCreateOrgs: true})
	svc := f.ssoService("bob@acme.com", "sub-bob", "acme.com")

	const n = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, n)
	joined := make([][]string, n)
	for i := range n {
		wg.Go(func() {
			<-start
			session, err := svc.CompleteOIDCSignIn(f.ctx, testOIDCProvider, coreoauth.AuthorizationCode{Code: "code"}, "", "")
			errs[i], joined[i] = err, session.JoinedOrgIDs
		})
	}
	close(start)
	wg.Wait()

	joins := 0
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("sign-in %d: %v", i, errs[i])
		}
		joins += len(joined[i])
	}
	if joins != 1 {
		t.Fatalf("%d sign-ins reported a join, want exactly 1", joins)
	}
	var customers int
	if err := f.db.PgW.QueryRow(f.ctx, "select count(*) from customers where lower(email) = 'bob@acme.com'").Scan(&customers); err != nil {
		t.Fatal(err)
	}
	if got := f.orgsOf("bob@acme.com"); customers != 1 || len(got) != 1 || got[orgID] == "" {
		t.Fatalf("customers = %d, orgs = %v; want one account in one org", customers, got)
	}
}

func TestInviteMagicLinkReportsTheJoinedOrg(t *testing.T) {
	f := newSSOFixture(t)
	orgID := f.org("acme.com", coreorgs.DomainSettings{MembersCanCreateOrgs: true})
	admins, err := f.read.GetOrgMembersByOrgID(f.ctx, orgID)
	if err != nil || len(admins) != 1 {
		t.Fatalf("admins = %v, %v", admins, err)
	}
	dispatch, err := f.orgs.InviteMemberWithRole(f.ctx, orgID, strings.TrimSpace(admins[0].CustomerID), "dana@acme.com", coreorgs.RoleMember)
	if err != nil {
		t.Fatalf("InviteMemberWithRole: %v", err)
	}

	svc := mustNewTestAuthService(t, f.db, &stubPublisher{})
	session, err := svc.CompleteMagicLink(f.ctx, dispatch.RawToken, "")
	if err != nil {
		t.Fatalf("CompleteMagicLink: %v", err)
	}
	if !slices.Equal(session.JoinedOrgIDs, []string{orgID}) {
		t.Fatalf("joined = %v, want %s", session.JoinedOrgIDs, orgID)
	}
}

func TestInviteMagicLinkForAnExistingMemberKeepsTheirRole(t *testing.T) {
	f := newSSOFixture(t)
	orgID := f.org("acme.com", coreorgs.DomainSettings{AutoJoinRole: coreorgs.RoleViewer, MembersCanCreateOrgs: true})
	admins, err := f.read.GetOrgMembersByOrgID(f.ctx, orgID)
	if err != nil || len(admins) != 1 {
		t.Fatalf("admins = %v, %v", admins, err)
	}
	dispatch, err := f.orgs.InviteMemberWithRole(f.ctx, orgID, strings.TrimSpace(admins[0].CustomerID), "bob@acme.com", coreorgs.RoleAdmin)
	if err != nil {
		t.Fatalf("InviteMemberWithRole: %v", err)
	}
	// The invite expires, SSO auto-joins bob as viewer, then the admin resends it.
	if _, err := f.db.PgW.Exec(f.ctx, "update org_invitations set expires_at = now() - interval '1 hour' where id = $1", dispatch.Invitation.ID); err != nil {
		t.Fatal(err)
	}
	f.signIn(f.ssoService("bob@acme.com", "sub-bob", "acme.com"))
	resent, err := f.orgs.ResendInvite(f.ctx, orgID, dispatch.Invitation.ID)
	if err != nil {
		t.Fatalf("ResendInvite: %v", err)
	}

	session, err := mustNewTestAuthService(t, f.db, &stubPublisher{}).CompleteMagicLink(f.ctx, resent.RawToken, "")
	if err != nil {
		t.Fatalf("CompleteMagicLink: %v", err)
	}
	if got := f.orgsOf("bob@acme.com"); len(session.JoinedOrgIDs) != 0 || got[orgID] != coreorgs.RoleViewer.String() {
		t.Fatalf("joined = %v, orgs = %v; want no join and the viewer role kept", session.JoinedOrgIDs, got)
	}
	inv, err := dbwrite.New(f.db.PgW).GetOrgInvitationByIDForUpdate(f.ctx, dispatch.Invitation.ID)
	if err != nil || inv.Status != orgsv1.InvitationStatus_INVITATION_STATUS_ACCEPTED.String() {
		t.Fatalf("invitation status = %q, %v; want ACCEPTED", inv.Status, err)
	}
}

// requireSSO turns Require SSO on or off for orgID's claim, marking the domain proven by SSO first.
func (f *ssoFixture) requireSSO(orgID, domain string, on bool) {
	f.t.Helper()
	if err := coreorgs.MarkSSOSeenInTx(f.ctx, dbwrite.New(f.db.PgW), domain); err != nil {
		f.t.Fatal(err)
	}
	_, domains, err := f.orgs.ListDomains(f.ctx, orgID)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, d := range domains {
		if d.Domain == domain {
			if _, err := f.orgs.UpdateDomain(f.ctx, orgID, d.ID, on); err != nil {
				f.t.Fatalf("UpdateDomain: %v", err)
			}
			return
		}
	}
	f.t.Fatalf("org %s has no claim on %s", orgID, domain)
}

func wantSSORequired(t *testing.T, err error, domain string, invite bool) {
	t.Helper()
	ssoErr, ok := errors.AsType[*coreorgs.SSORequiredError](err)
	if !ok || ssoErr.Domain != domain || ssoErr.Invite != invite {
		t.Fatalf("err = %v, want SSORequiredError for %s with invite %v", err, domain, invite)
	}
}

func TestRequireSSOBlocksPasswordsAndEmailLinks(t *testing.T) {
	f := newSSOFixture(t)
	orgID := f.org("acme.com", coreorgs.DomainSettings{MembersCanCreateOrgs: true})
	pub := &stubPublisher{}
	svc := mustNewTestAuthService(t, f.db, pub)
	passwordSession := seedSignedInCustomer(t, svc, dbwrite.New(f.db.PgW), xid.New().String(), "bob@acme.com")
	if err := svc.RequestMagicLink(f.ctx, "bob@acme.com"); err != nil {
		t.Fatal(err)
	}
	link := lastMagicToken(t, pub)
	linkSession, err := svc.CompleteMagicLink(f.ctx, link, "")
	if err != nil {
		t.Fatal(err)
	}
	// Rotated, so the first token can be replayed below.
	rotated, err := svc.RefreshSession(f.ctx, linkSession.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RequestMagicLink(f.ctx, "bob@acme.com"); err != nil {
		t.Fatal(err)
	}
	link = lastMagicToken(t, pub)
	sent := len(pub.jobs)

	f.requireSSO(orgID, "acme.com", true)

	_, err = svc.SignInWithEmail(f.ctx, "bob@acme.com", "password123")
	wantSSORequired(t, err, "acme.com", false)
	// Checked before the account lookup, so it can't tell whether an account exists.
	_, err = svc.SignInWithEmail(f.ctx, "nobody@acme.com", "wrong")
	wantSSORequired(t, err, "acme.com", false)
	wantSSORequired(t, svc.RequestMagicLink(f.ctx, "nobody@acme.com"), "acme.com", false)
	if len(pub.jobs) != sent {
		t.Fatal("a refused magic-link request sent an email")
	}
	_, err = svc.CompleteMagicLink(f.ctx, link, "")
	wantSSORequired(t, err, "acme.com", false)
	for _, session := range []coreauth.Session{passwordSession, rotated} {
		_, err = svc.RefreshSession(f.ctx, session.RefreshToken)
		wantSSORequired(t, err, "acme.com", false)
	}
	// Reuse detection runs before the SSO check.
	if _, err := svc.RefreshSession(f.ctx, linkSession.RefreshToken); !errors.Is(err, coreauth.ErrInvalidToken) {
		t.Fatalf("replayed token err = %v, want ErrInvalidToken", err)
	}
	if err := svc.RequestMagicLink(f.ctx, "carol@globex.com"); err != nil {
		t.Fatalf("another domain: %v", err)
	}

	// The refused link was left unused, so turning it off brings it back.
	f.requireSSO(orgID, "acme.com", false)
	if _, err := svc.CompleteMagicLink(f.ctx, link, ""); err != nil {
		t.Fatalf("the refused link was used up: %v", err)
	}
	// The refused session was revoked: the frontend drops it, so only a copy could come back.
	if _, err := svc.RefreshSession(f.ctx, passwordSession.RefreshToken); !errors.Is(err, coreauth.ErrInvalidToken) {
		t.Fatalf("refused session err = %v, want ErrInvalidToken", err)
	}
}

// The frontend ends a session only on the refusal, so a failed revoke must not replace it.
func TestRefusedRefreshSurvivesAFailedRevoke(t *testing.T) {
	f := newSSOFixture(t)
	orgID := f.org("acme.com", coreorgs.DomainSettings{MembersCanCreateOrgs: true})
	svc := mustNewTestAuthService(t, f.db, &stubPublisher{})
	session := seedSignedInCustomer(t, svc, dbwrite.New(f.db.PgW), xid.New().String(), "bob@acme.com")
	f.requireSSO(orgID, "acme.com", true)

	// Each test has its own database, so the trigger stays in this one.
	for _, stmt := range []string{
		"create function refuse_update() returns trigger language plpgsql as $$ begin raise exception 'refused'; end $$",
		"create trigger refuse_update before update on refresh_tokens for each row execute function refuse_update()",
	} {
		if _, err := f.db.PgW.Exec(f.ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	_, err := svc.RefreshSession(f.ctx, session.RefreshToken)
	wantSSORequired(t, err, "acme.com", false)
}

func TestRequireSSOAtOIDCSignIn(t *testing.T) {
	f := newSSOFixture(t)
	orgID := f.org("acme.com", coreorgs.DomainSettings{MembersCanCreateOrgs: true})
	f.signIn(f.ssoService("bob@acme.com", "sub-bob", "acme.com"))
	if claims, err := f.orgs.DomainClaims(f.ctx, "acme.com"); err != nil || len(claims) != 1 || !claims[0].SsoSeenAt.Valid {
		t.Fatalf("claims = %+v, %v; want sso_seen_at set by the SSO sign-in", claims, err)
	}
	f.requireSSO(orgID, "acme.com", true)

	session := f.signIn(f.ssoService("bob@acme.com", "sub-bob", "acme.com"))
	if _, err := mustNewTestAuthService(t, f.db, &stubPublisher{}).RefreshSession(f.ctx, session.RefreshToken); err != nil {
		t.Fatalf("an SSO session was not renewed: %v", err)
	}

	signIn := func(email, sub string) error {
		_, err := f.ssoService(email, sub, "").CompleteOIDCSignIn(f.ctx, testOIDCProvider, coreoauth.AuthorizationCode{Code: "code"}, "", "")
		return err
	}
	// A personal account with an address on the domain proves nothing.
	wantSSORequired(t, signIn("carol@acme.com", "sub-carol"), "acme.com", false)
	if _, err := f.read.GetCustomerByEmail(f.ctx, "carol@acme.com"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("a refused sign-in created an account: %v", err)
	}
	// Sign-in resolves sub first, so a token for another address still reaches bob's account.
	wantSSORequired(t, signIn("bob@gmail.com", "sub-bob"), "acme.com", false)
	// And the token's own address counts even when its sub is linked to an account elsewhere.
	if err := signIn("dave@globex.com", "sub-dave"); err != nil {
		t.Fatal(err)
	}
	wantSSORequired(t, signIn("dave@acme.com", "sub-dave"), "acme.com", false)
}

func TestRequireSSOExemptsTheDemoViewer(t *testing.T) {
	f := newSSOFixture(t)
	orgID := f.org("pug.sh", coreorgs.DomainSettings{MembersCanCreateOrgs: true})
	w := dbwrite.New(f.db.PgW)
	viewer, err := w.CreateCustomer(f.ctx, dbwrite.CreateCustomerParams{ID: xid.New().String(), Email: coreauth.DemoViewerEmail})
	if err != nil {
		t.Fatal(err)
	}
	demoOrg, err := w.CreateOrg(f.ctx, dbwrite.CreateOrgParams{ID: xid.New().String(), DisplayName: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateOrgMember(f.ctx, dbwrite.CreateOrgMemberParams{
		OrgID: demoOrg.ID, CustomerID: viewer.ID, Role: coreorgs.RoleViewer.String(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := projects.CreateProjectInTx(f.ctx, w, demoOrg.ID, "default", ""); err != nil {
		t.Fatal(err)
	}
	f.requireSSO(orgID, "pug.sh", true)

	svc := mustNewTestAuthService(t, f.db, &stubPublisher{})
	svc.SetDemoEnabledForTest(true)
	demo, err := svc.DemoSignIn(f.ctx)
	if err != nil {
		t.Fatalf("DemoSignIn: %v", err)
	}
	if _, err := svc.RefreshSession(f.ctx, demo.Session.RefreshToken); err != nil {
		t.Fatalf("demo session refresh: %v", err)
	}
}

func TestInviteAcceptedThroughSSO(t *testing.T) {
	f := newSSOFixture(t)
	orgID := f.org("acme.com", coreorgs.DomainSettings{MembersCanCreateOrgs: true})
	pub := &stubPublisher{}
	svc := mustNewTestAuthService(t, f.db, pub)
	if err := svc.RequestMagicLink(f.ctx, "carol@acme.com"); err != nil {
		t.Fatal(err)
	}
	loginLink := lastMagicToken(t, pub)
	admins, err := f.read.GetOrgMembersByOrgID(f.ctx, orgID)
	if err != nil || len(admins) != 1 {
		t.Fatalf("admins = %v, %v", admins, err)
	}
	invite, err := f.orgs.InviteMemberWithRole(f.ctx, orgID, strings.TrimSpace(admins[0].CustomerID), "carol@acme.com", coreorgs.RoleMember)
	if err != nil {
		t.Fatalf("InviteMemberWithRole: %v", err)
	}
	f.requireSSO(orgID, "acme.com", true)

	_, err = svc.CompleteMagicLink(f.ctx, invite.RawToken, "")
	wantSSORequired(t, err, "acme.com", true)
	// A personal account proves nothing, and its refusal still carries the invite.
	_, err = f.ssoService("carol@acme.com", "sub-personal", "").CompleteOIDCSignIn(f.ctx, testOIDCProvider, coreoauth.AuthorizationCode{Code: "code"}, invite.RawToken, "")
	wantSSORequired(t, err, "acme.com", true)

	signIn := func(email, sub, inviteToken string) (coreauth.Session, error) {
		return f.ssoService(email, sub, "acme.com").CompleteOIDCSignIn(f.ctx, testOIDCProvider, coreoauth.AuthorizationCode{Code: "code"}, inviteToken, "")
	}
	if _, err := signIn("dave@acme.com", "sub-dave", invite.RawToken); !errors.Is(err, coreauth.ErrInviteWrongEmail) {
		t.Fatalf("another account's err = %v, want ErrInviteWrongEmail", err)
	}
	if _, err := f.read.GetCustomerByEmail(f.ctx, "dave@acme.com"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("a refused invite left an account behind: %v", err)
	}
	// The invite is matched against the account sub resolves to, not the token's email.
	if _, err := signIn("dave@acme.com", "sub-dave", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := signIn("carol@acme.com", "sub-dave", invite.RawToken); !errors.Is(err, coreauth.ErrInviteWrongEmail) {
		t.Fatalf("dave's account err = %v, want ErrInviteWrongEmail", err)
	}
	for _, token := range []string{"not-a-token", loginLink} {
		if _, err := signIn("carol@acme.com", "sub-carol", token); !errors.Is(err, coreauth.ErrInvalidToken) {
			t.Fatalf("invite token %q err = %v, want ErrInvalidToken", token, err)
		}
	}

	session, err := signIn("Carol@acme.com", "sub-carol", invite.RawToken)
	if err != nil {
		t.Fatalf("CompleteOIDCSignIn: %v", err)
	}
	if !slices.Equal(session.JoinedOrgIDs, []string{orgID}) {
		t.Fatalf("joined = %v, want %s", session.JoinedOrgIDs, orgID)
	}
	if got := f.orgsOf("carol@acme.com"); got[orgID] != coreorgs.RoleMember.String() {
		t.Fatalf("orgs = %v, want %s as member", got, orgID)
	}
	if _, err := signIn("carol@acme.com", "sub-carol", invite.RawToken); !errors.Is(err, coreauth.ErrInvalidToken) {
		t.Fatalf("reused invite err = %v, want ErrInvalidToken", err)
	}
}

// An expired invite is refused on both paths while its link is still valid, and
// leaves no account behind.
func TestExpiredInviteIsRefused(t *testing.T) {
	f := newSSOFixture(t)
	orgID := f.org("acme.com", coreorgs.DomainSettings{MembersCanCreateOrgs: true})
	admins, err := f.read.GetOrgMembersByOrgID(f.ctx, orgID)
	if err != nil || len(admins) != 1 {
		t.Fatalf("admins = %v, %v", admins, err)
	}
	invite, err := f.orgs.InviteMemberWithRole(f.ctx, orgID, strings.TrimSpace(admins[0].CustomerID), "carol@acme.com", coreorgs.RoleMember)
	if err != nil {
		t.Fatalf("InviteMemberWithRole: %v", err)
	}
	if _, err := f.db.PgW.Exec(f.ctx, "update org_invitations set expires_at = now() - interval '1 hour' where id = $1", invite.Invitation.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := mustNewTestAuthService(t, f.db, &stubPublisher{}).CompleteMagicLink(f.ctx, invite.RawToken, ""); !errors.Is(err, coreauth.ErrInvalidToken) {
		t.Fatalf("magic link err = %v, want ErrInvalidToken", err)
	}
	_, err = f.ssoService("carol@acme.com", "sub-carol", "acme.com").CompleteOIDCSignIn(f.ctx, testOIDCProvider, coreoauth.AuthorizationCode{Code: "code"}, invite.RawToken, "")
	if !errors.Is(err, coreauth.ErrInvalidToken) {
		t.Fatalf("sso err = %v, want ErrInvalidToken", err)
	}
	if _, err := f.read.GetCustomerByEmail(f.ctx, "carol@acme.com"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("a refused invite left an account behind: %v", err)
	}
}
