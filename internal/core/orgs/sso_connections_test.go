package orgs_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"code.dny.dev/ssrf"
	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/rs/xid"

	coreoauth "github.com/pug-sh/pug/internal/core/auth/oauth"
	"github.com/pug-sh/pug/internal/core/email/secret"
	"github.com/pug-sh/pug/internal/core/orgs"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
)

const testIssuer = "https://acme.okta.com"

func newSSOConnectionFixture(t *testing.T) (*domainFixture, *secret.Cipher, *[]string) {
	t.Helper()
	f := newDomainFixture(t)
	cipher, err := secret.NewCipher(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))))
	if err != nil {
		t.Fatal(err)
	}
	var discovered []string
	f.svc.WithSSOConnections(cipher, func(_ context.Context, issuer string) error {
		discovered = append(discovered, issuer)
		switch {
		case strings.Contains(issuer, "broken"):
			return errors.New("no discovery document")
		case strings.Contains(issuer, "mismatch"):
			return fmt.Errorf("discover: %w", &oidc.IssuerMismatchError{Provided: issuer, Discovered: issuer + "/"})
		case strings.Contains(issuer, "private"):
			return fmt.Errorf("discover: %w", ssrf.ErrProhibitedIP)
		}
		return nil
	})
	return f, cipher, &discovered
}

func (f *domainFixture) connection(orgID string, in orgs.SSOConnectionInput) (orgs.SSOConnection, error) {
	f.t.Helper()
	if in.Label == "" {
		in.Label = "Acme SSO"
	}
	if in.IssuerURL == "" {
		in.IssuerURL = testIssuer
	}
	if in.ClientID == "" {
		in.ClientID = "client"
	}
	if in.ID == "" && in.ClientSecret == "" {
		in.ClientSecret = "secret"
	}
	return f.svc.SetSSOConnection(f.ctx, orgID, in)
}

func (f *domainFixture) mustConnection(orgID string, in orgs.SSOConnectionInput) orgs.SSOConnection {
	f.t.Helper()
	c, err := f.connection(orgID, in)
	if err != nil {
		f.t.Fatalf("SetSSOConnection: %v", err)
	}
	return c
}

func TestSSOConnectionsOffWithoutAKey(t *testing.T) {
	f := newDomainFixture(t)
	orgID, _ := f.org("admin@acme.com")
	d := f.verifiedDomain(orgID, "acme.com")
	if _, err := f.connection(orgID, orgs.SSOConnectionInput{DomainIDs: []string{d.ID}}); !errors.Is(err, orgs.ErrSSOConnectionsDisabled) {
		t.Fatalf("set: err = %v, want ErrSSOConnectionsDisabled", err)
	}
	if _, err := f.svc.ListSSOConnections(f.ctx, orgID); !errors.Is(err, orgs.ErrSSOConnectionsDisabled) {
		t.Fatalf("list: err = %v, want ErrSSOConnectionsDisabled", err)
	}
	if err := f.svc.DeleteSSOConnection(f.ctx, orgID, xid.New().String()); !errors.Is(err, orgs.ErrSSOConnectionsDisabled) {
		t.Fatalf("delete: err = %v, want ErrSSOConnectionsDisabled", err)
	}
}

func TestSSOConnectionNeedsTheOrgsVerifiedDomains(t *testing.T) {
	f, _, _ := newSSOConnectionFixture(t)
	orgID, _ := f.org("admin@acme.com")
	pending := f.addDomain(orgID, "acme.com")
	otherOrg, _ := f.org("admin@globex.com")
	foreign := f.verifiedDomain(otherOrg, "globex.com")

	if _, err := f.connection(orgID, orgs.SSOConnectionInput{DomainIDs: []string{pending.ID}}); !errors.Is(err, orgs.ErrDomainNotVerified) {
		t.Fatalf("pending domain err = %v, want ErrDomainNotVerified", err)
	}
	if _, err := f.connection(orgID, orgs.SSOConnectionInput{DomainIDs: []string{foreign.ID}}); !errors.Is(err, orgs.ErrDomainNotFound) {
		t.Fatalf("another org's domain err = %v, want ErrDomainNotFound", err)
	}
}

func TestSSOConnectionIssuerChecks(t *testing.T) {
	f, _, discovered := newSSOConnectionFixture(t)
	orgID, _ := f.org("admin@acme.com")
	d := f.verifiedDomain(orgID, "acme.com")

	for issuer, want := range map[string]error{
		"https://accounts.google.com":  orgs.ErrSSOConnectionGoogleIssuer,
		"https://accounts.google.com/": orgs.ErrSSOConnectionGoogleIssuer,
		"http://acme.okta.com":         orgs.ErrSSOConnectionIssuerInvalid,
		"https://broken.example.com":   orgs.ErrSSOConnectionDiscovery,
		"https://mismatch.example.com": orgs.ErrSSOConnectionIssuerMismatch,
		"https://private.example.com":  orgs.ErrSSOConnectionPrivateIssuer,
	} {
		if _, err := f.connection(orgID, orgs.SSOConnectionInput{IssuerURL: issuer, DomainIDs: []string{d.ID}}); !errors.Is(err, want) {
			t.Fatalf("issuer %s: err = %v, want %v", issuer, err, want)
		}
	}
	c := f.mustConnection(orgID, orgs.SSOConnectionInput{DomainIDs: []string{d.ID}})
	if c.IssuerURL != testIssuer || len(c.Domains) != 1 || c.Domains[0].Domain != "acme.com" {
		t.Fatalf("connection = %+v", c)
	}
	if (*discovered)[len(*discovered)-1] != testIssuer {
		t.Fatalf("discovered = %v, want the issuer fetched on save", *discovered)
	}
}

func TestSSOConnectionOnePerDomain(t *testing.T) {
	f, _, _ := newSSOConnectionFixture(t)
	acme, _ := f.org("admin@acme.com")
	acmeDomain := f.verifiedDomain(acme, "acme.com")
	globexDomain := f.verifiedDomain(acme, "globex.com")
	f.mustConnection(acme, orgs.SSOConnectionInput{DomainIDs: []string{acmeDomain.ID}})

	if _, err := f.connection(acme, orgs.SSOConnectionInput{DomainIDs: []string{acmeDomain.ID}}); !errors.Is(err, orgs.ErrDomainHasSSOConnection) {
		t.Fatalf("second connection in the same org: err = %v, want ErrDomainHasSSOConnection", err)
	}
	other, _ := f.org("admin@other.com")
	otherClaim := f.verifiedDomain(other, "acme.com")
	if _, err := f.connection(other, orgs.SSOConnectionInput{DomainIDs: []string{otherClaim.ID}}); !errors.Is(err, orgs.ErrDomainHasSSOConnection) {
		t.Fatalf("connection from another org: err = %v, want ErrDomainHasSSOConnection", err)
	}
	f.mustConnection(acme, orgs.SSOConnectionInput{Label: "Globex SSO", IssuerURL: "https://globex.okta.com", DomainIDs: []string{globexDomain.ID}})

	conns, err := f.svc.ListSSOConnections(f.ctx, acme)
	if err != nil || len(conns) != 2 || conns[0].Domains[0].Domain != "acme.com" || conns[1].Domains[0].Domain != "globex.com" {
		t.Fatalf("ListSSOConnections = %+v, %v; want acme.com's and globex.com's", conns, err)
	}
	_, domains, err := f.svc.ListDomains(f.ctx, other)
	if err != nil || len(domains) != 1 || !domains[0].SSOConnectionElsewhere || domains[0].SSOConnectionID != "" {
		t.Fatalf("other org's domains = %+v, %v; want the connection shown as elsewhere", domains, err)
	}
	_, domains, err = f.svc.ListDomains(f.ctx, acme)
	if err != nil || domains[0].SSOConnectionID == "" || domains[0].SSOConnectionElsewhere {
		t.Fatalf("acme's domains = %+v, %v; want its own connection", domains, err)
	}
}

func TestSSOConnectionRechecksTheRecord(t *testing.T) {
	f, _, _ := newSSOConnectionFixture(t)
	orgID, _ := f.org("admin@acme.com")
	acme := f.verifiedDomain(orgID, "acme.com")
	globex := f.verifiedDomain(orgID, "globex.com")
	c := f.mustConnection(orgID, orgs.SSOConnectionInput{DomainIDs: []string{acme.ID}})

	f.dns.unpublish(acme)
	f.dns.unpublish(globex)
	// Same issuer and domains: nothing to re-check.
	if _, err := f.connection(orgID, orgs.SSOConnectionInput{ID: c.ID, Label: "Renamed", DomainIDs: []string{acme.ID}}); err != nil {
		t.Fatalf("relabel: %v", err)
	}
	_, err := f.connection(orgID, orgs.SSOConnectionInput{ID: c.ID, DomainIDs: []string{acme.ID, globex.ID}})
	wantVerificationError(t, err, "globex.com")
	_, err = f.connection(orgID, orgs.SSOConnectionInput{ID: c.ID, IssuerURL: "https://new.okta.com", ClientSecret: "new-secret", DomainIDs: []string{acme.ID}})
	wantVerificationError(t, err, "acme.com")

	operator, err := f.svc.VerifyDomainByOperator(f.ctx, orgID, "initech.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.connection(orgID, orgs.SSOConnectionInput{ID: c.ID, DomainIDs: []string{acme.ID, operator.ID}}); err != nil {
		t.Fatalf("operator-verified domain must skip the check: %v", err)
	}
}

func TestSSOConnectionCantGoAwayWhileSSOIsRequired(t *testing.T) {
	f, _, _ := newSSOConnectionFixture(t)
	orgID, _ := f.org("admin@acme.com")
	acme := f.verifiedDomain(orgID, "acme.com")
	globex := f.verifiedDomain(orgID, "globex.com")
	c := f.mustConnection(orgID, orgs.SSOConnectionInput{DomainIDs: []string{acme.ID, globex.ID}})

	// globex.com sorts last, so every check has to reach past the first domain.
	other, _ := f.org("admin@other.com")
	otherClaim := f.verifiedDomain(other, "globex.com")
	f.ssoSeen("globex.com")
	f.requireSSO(other, otherClaim, true)

	wantInUse := func(err error) {
		t.Helper()
		inUse, ok := errors.AsType[*orgs.SSOConnectionInUseError](err)
		if !ok || inUse.Domain != "globex.com" {
			t.Fatalf("err = %v, want SSOConnectionInUseError for globex.com", err)
		}
	}
	wantInUse(f.svc.DeleteSSOConnection(f.ctx, orgID, c.ID))
	_, err := f.connection(orgID, orgs.SSOConnectionInput{ID: c.ID, DomainIDs: []string{acme.ID}})
	wantInUse(err)
	wantInUse(f.svc.RemoveDomain(f.ctx, orgID, globex.ID))
	if _, err := f.connection(orgID, orgs.SSOConnectionInput{ID: c.ID, DomainIDs: []string{globex.ID}}); err != nil {
		t.Fatalf("dropping a domain that doesn't require SSO: %v", err)
	}

	f.requireSSO(other, otherClaim, false)
	if err := f.svc.DeleteSSOConnection(f.ctx, orgID, c.ID); err != nil {
		t.Fatalf("DeleteSSOConnection: %v", err)
	}
	// With the connection gone, Require SSO waits for a new SSO sign-in.
	if _, err := f.svc.UpdateDomain(f.ctx, other, otherClaim.ID, true); !errors.Is(err, orgs.ErrDomainSSONotSeen) {
		t.Fatalf("Require SSO after the delete: err = %v, want ErrDomainSSONotSeen", err)
	}
	_, domains, err := f.svc.ListDomains(f.ctx, orgID)
	i := slices.IndexFunc(domains, func(d orgs.Domain) bool { return d.ID == globex.ID })
	if err != nil || i < 0 || !domains[i].Verified() || domains[i].SSOConnectionID != "" {
		t.Fatalf("domains = %+v, %v; want globex.com kept and verified, with no connection", domains, err)
	}
	if err := f.svc.DeleteSSOConnection(f.ctx, orgID, c.ID); !errors.Is(err, orgs.ErrSSOConnectionNotFound) {
		t.Fatalf("second delete err = %v, want ErrSSOConnectionNotFound", err)
	}
}

func TestSSOConnectionIssuerChangeUnlinksIdentities(t *testing.T) {
	f, cipher, _ := newSSOConnectionFixture(t)
	orgID, _ := f.org("admin@acme.com")
	acme := f.verifiedDomain(orgID, "acme.com")
	c := f.mustConnection(orgID, orgs.SSOConnectionInput{DomainIDs: []string{acme.ID}})
	bob := f.customer("bob@acme.com")
	link := func() {
		t.Helper()
		if _, err := f.w.CreateCustomerIdentity(f.ctx, dbwrite.CreateCustomerIdentityParams{
			ID: xid.New().String(), CustomerID: bob, Provider: string(coreoauth.ConnectionProviderName(c.ID)), ProviderSubject: "sub-bob",
		}); err != nil {
			t.Fatal(err)
		}
	}
	linked := func() bool {
		t.Helper()
		var n int
		if err := f.db.PgW.QueryRow(f.ctx, "select count(*) from customer_identities where customer_id = $1", bob).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n > 0
	}
	link()

	f.mustConnection(orgID, orgs.SSOConnectionInput{ID: c.ID, Label: "Renamed", DomainIDs: []string{acme.ID}})
	if !linked() {
		t.Fatal("a change that keeps the issuer must keep linked identities")
	}
	conn, err := orgs.SSOConnectionForSignIn(f.ctx, f.w, cipher, c.ID)
	if err != nil || conn.ClientSecret != "secret" || conn.Label != "Renamed" {
		t.Fatalf("an empty secret must keep the stored one: %+v, %v", conn, err)
	}
	// A secret belongs to one client.
	if _, err := f.connection(orgID, orgs.SSOConnectionInput{ID: c.ID, ClientID: "other-client", DomainIDs: []string{acme.ID}}); !errors.Is(err, orgs.ErrSSOConnectionSecretRequired) {
		t.Fatalf("new client id without a secret: err = %v, want ErrSSOConnectionSecretRequired", err)
	}
	// The stored secret was issued for the old issuer and must not reach a new one.
	if _, err := f.connection(orgID, orgs.SSOConnectionInput{ID: c.ID, IssuerURL: "https://new.okta.com", DomainIDs: []string{acme.ID}}); !errors.Is(err, orgs.ErrSSOConnectionSecretRequired) {
		t.Fatalf("new issuer without a secret: err = %v, want ErrSSOConnectionSecretRequired", err)
	}
	f.mustConnection(orgID, orgs.SSOConnectionInput{ID: c.ID, IssuerURL: "https://new.okta.com", ClientSecret: "new-secret", DomainIDs: []string{acme.ID}})
	if linked() {
		t.Fatal("a new issuer must drop the connection's linked identities")
	}
	conn, err = orgs.SSOConnectionForSignIn(f.ctx, f.w, cipher, c.ID)
	if err != nil || conn.ClientSecret != "new-secret" {
		t.Fatalf("SSOConnectionForSignIn = %+v, %v", conn, err)
	}

	link()
	f.requireSSO(orgID, acme, false)
	if err := f.svc.DeleteSSOConnection(f.ctx, orgID, c.ID); err != nil {
		t.Fatal(err)
	}
	if linked() {
		t.Fatal("deleting the connection must drop its linked identities")
	}
}

// Another org can't list, change or delete the connection, and its linked accounts stay linked.
func TestSSOConnectionIsTheOrgsOwn(t *testing.T) {
	f, _, _ := newSSOConnectionFixture(t)
	acme, _ := f.org("admin@acme.com")
	c := f.mustConnection(acme, orgs.SSOConnectionInput{DomainIDs: []string{f.verifiedDomain(acme, "acme.com").ID}})
	bob := f.customer("bob@acme.com")
	if _, err := f.w.CreateCustomerIdentity(f.ctx, dbwrite.CreateCustomerIdentityParams{
		ID: xid.New().String(), CustomerID: bob, Provider: string(coreoauth.ConnectionProviderName(c.ID)), ProviderSubject: "sub-bob",
	}); err != nil {
		t.Fatal(err)
	}
	other, _ := f.org("admin@other.com")
	otherDomain := f.verifiedDomain(other, "other.com")

	if conns, err := f.svc.ListSSOConnections(f.ctx, other); err != nil || len(conns) != 0 {
		t.Fatalf("another org's list = %+v, %v; want none", conns, err)
	}
	if err := f.svc.DeleteSSOConnection(f.ctx, other, c.ID); !errors.Is(err, orgs.ErrSSOConnectionNotFound) {
		t.Fatalf("delete another org's connection: err = %v, want ErrSSOConnectionNotFound", err)
	}
	if _, err := f.connection(other, orgs.SSOConnectionInput{ID: c.ID, ClientSecret: "secret", DomainIDs: []string{otherDomain.ID}}); !errors.Is(err, orgs.ErrSSOConnectionNotFound) {
		t.Fatalf("set another org's connection: err = %v, want ErrSSOConnectionNotFound", err)
	}
	var n int
	if err := f.db.PgW.QueryRow(f.ctx, "select count(*) from customer_identities where customer_id = $1", bob).Scan(&n); err != nil || n != 1 {
		t.Fatalf("bob's identities = %d, %v; want 1", n, err)
	}
}

func TestSSOConnectionLimit(t *testing.T) {
	f, _, _ := newSSOConnectionFixture(t)
	orgID, _ := f.org("admin@acme.com")
	for range 10 {
		f.mustConnection(orgID, orgs.SSOConnectionInput{})
	}
	if _, err := f.connection(orgID, orgs.SSOConnectionInput{}); !errors.Is(err, orgs.ErrSSOConnectionLimitReached) {
		t.Fatalf("11th connection: err = %v, want ErrSSOConnectionLimitReached", err)
	}
	if _, err := f.connection(xid.New().String(), orgs.SSOConnectionInput{}); !errors.Is(err, orgs.ErrOrgNotFound) {
		t.Fatalf("unknown org: err = %v, want ErrOrgNotFound", err)
	}
}

// Taking a domain off a connection, or removing or releasing the claim that carries it,
// means Require SSO waits for a new SSO sign-in: nothing may be left to sign the domain in.
func TestSSOConnectionDomainGoneNeedsANewSSOSignIn(t *testing.T) {
	f, _, _ := newSSOConnectionFixture(t)
	orgID, _ := f.org("admin@acme.com")
	acme := f.verifiedDomain(orgID, "acme.com")
	globex := f.verifiedDomain(orgID, "globex.com")
	initech := f.verifiedDomain(orgID, "initech.com")
	c := f.mustConnection(orgID, orgs.SSOConnectionInput{DomainIDs: []string{acme.ID, globex.ID, initech.ID}})
	other, _ := f.org("admin@other.com")
	otherAcme := f.verifiedDomain(other, "acme.com")
	otherGlobex := f.verifiedDomain(other, "globex.com")
	otherInitech := f.verifiedDomain(other, "initech.com")
	for _, d := range []string{"acme.com", "globex.com", "initech.com"} {
		f.ssoSeen(d)
	}
	wantNotSeen := func(d orgs.Domain) {
		t.Helper()
		if _, err := f.svc.UpdateDomain(f.ctx, other, d.ID, true); !errors.Is(err, orgs.ErrDomainSSONotSeen) {
			t.Fatalf("Require SSO for %s: err = %v, want ErrDomainSSONotSeen", d.Domain, err)
		}
	}

	f.mustConnection(orgID, orgs.SSOConnectionInput{ID: c.ID, DomainIDs: []string{acme.ID, initech.ID}})
	wantNotSeen(otherGlobex)
	if err := f.svc.RemoveDomain(f.ctx, orgID, acme.ID); err != nil {
		t.Fatal(err)
	}
	wantNotSeen(otherAcme)
	if err := f.svc.ReleaseDomain(f.ctx, orgID, "initech.com"); err != nil {
		t.Fatal(err)
	}
	wantNotSeen(otherInitech)
}

// A new issuer hasn't signed anyone in yet, so Require SSO waits for a sign-in through it.
// A change that keeps the issuer keeps the earlier sign-in.
func TestSSOConnectionNewIssuerNeedsANewSSOSignIn(t *testing.T) {
	f, _, _ := newSSOConnectionFixture(t)
	orgID, _ := f.org("admin@acme.com")
	acme := f.verifiedDomain(orgID, "acme.com")
	c := f.mustConnection(orgID, orgs.SSOConnectionInput{DomainIDs: []string{acme.ID}})
	f.ssoSeen("acme.com")

	f.mustConnection(orgID, orgs.SSOConnectionInput{ID: c.ID, Label: "Renamed", DomainIDs: []string{acme.ID}})
	f.requireSSO(orgID, acme, true)
	f.requireSSO(orgID, acme, false)
	f.mustConnection(orgID, orgs.SSOConnectionInput{ID: c.ID, IssuerURL: "https://new.okta.com", ClientSecret: "new-secret", DomainIDs: []string{acme.ID}})
	if _, err := f.svc.UpdateDomain(f.ctx, orgID, acme.ID, true); !errors.Is(err, orgs.ErrDomainSSONotSeen) {
		t.Fatalf("Require SSO after a new issuer: err = %v, want ErrDomainSSONotSeen", err)
	}
}

// Require SSO turned on while a save checks its issuer still keeps the domain on the connection.
func TestSSOConnectionRequireSSODuringASave(t *testing.T) {
	f, cipher, _ := newSSOConnectionFixture(t)
	orgID, _ := f.org("admin@acme.com")
	acme := f.verifiedDomain(orgID, "acme.com")
	globex := f.verifiedDomain(orgID, "globex.com")
	c := f.mustConnection(orgID, orgs.SSOConnectionInput{DomainIDs: []string{acme.ID, globex.ID}})
	other, _ := f.org("admin@other.com")
	otherClaim := f.verifiedDomain(other, "globex.com")
	f.ssoSeen("globex.com")
	f.svc.WithSSOConnections(cipher, func(context.Context, string) error {
		f.requireSSO(other, otherClaim, true)
		return nil
	})

	_, err := f.connection(orgID, orgs.SSOConnectionInput{ID: c.ID, DomainIDs: []string{acme.ID}})
	if inUse, ok := errors.AsType[*orgs.SSOConnectionInUseError](err); !ok || inUse.Domain != "globex.com" {
		t.Fatalf("err = %v, want SSOConnectionInUseError for globex.com", err)
	}
}

// An update leaves the org unlocked. A sign-in holds the connection and then locks the org
// through auto-join, so an update that locked the org first would deadlock with it.
func TestSSOConnectionUpdateLeavesTheOrgUnlocked(t *testing.T) {
	f, cipher, _ := newSSOConnectionFixture(t)
	orgID, _ := f.org("admin@acme.com")
	acme := f.verifiedDomain(orgID, "acme.com")
	c := f.mustConnection(orgID, orgs.SSOConnectionInput{DomainIDs: []string{acme.ID}})

	signIn, err := f.db.PgW.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = signIn.Rollback(f.ctx) }()
	held := make(chan error, 1)
	// Runs after the update's reads and before its transaction.
	f.svc.WithSSOConnections(cipher, func(context.Context, string) error {
		_, err := signIn.Exec(f.ctx, "select 1 from org_sso_connections where id = $1 for share", c.ID)
		held <- err
		return err
	})
	done := make(chan error, 1)
	go func() {
		_, err := f.svc.SetSSOConnection(f.ctx, orgID, orgs.SSOConnectionInput{
			ID: c.ID, Label: "Renamed", IssuerURL: testIssuer, ClientID: "client", DomainIDs: []string{acme.ID},
		})
		done <- err
	}()
	if err := <-held; err != nil {
		t.Fatal(err)
	}
	f.waitForBlocked(done)
	// As auto-join's foreign key does.
	if _, err := signIn.Exec(f.ctx, "select 1 from orgs where id = $1 for key share", orgID); err != nil {
		t.Fatalf("sign-in's org lock: %v", err)
	}
	if err := signIn.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("update: %v", err)
	}
}

// RemoveDomain waits for a sign-in through the domain's connection, so the sign-in can't
// mark the domain SSO-seen again after the clear.
func TestRemoveDomainWaitsForSignIns(t *testing.T) {
	f, _, _ := newSSOConnectionFixture(t)
	orgID, _ := f.org("admin@acme.com")
	acme := f.verifiedDomain(orgID, "acme.com")
	c := f.mustConnection(orgID, orgs.SSOConnectionInput{DomainIDs: []string{acme.ID}})
	other, _ := f.org("admin@other.com")
	otherAcme := f.verifiedDomain(other, "acme.com")

	// A sign-in past its re-check, about to mark the domain seen.
	signIn, err := f.db.PgW.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = signIn.Rollback(f.ctx) }()
	if _, err := signIn.Exec(f.ctx, "select 1 from org_sso_connections where id = $1 for share", c.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- f.svc.RemoveDomain(f.ctx, orgID, acme.ID) }()
	f.waitForBlocked(done)
	if err := orgs.MarkSSOSeenInTx(f.ctx, dbwrite.New(signIn), "acme.com"); err != nil {
		t.Fatal(err)
	}
	if err := signIn.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("RemoveDomain: %v", err)
	}
	if _, err := f.svc.UpdateDomain(f.ctx, other, otherAcme.ID, true); !errors.Is(err, orgs.ErrDomainSSONotSeen) {
		t.Fatalf("Require SSO after the removal: err = %v, want ErrDomainSSONotSeen", err)
	}
}

// The delete takes only connection identities, whatever provider it is given.
func TestDeleteSSOConnectionIdentitiesSparesOtherProviders(t *testing.T) {
	f := newDomainFixture(t)
	bob := f.customer("bob@acme.com")
	if _, err := f.w.CreateCustomerIdentity(f.ctx, dbwrite.CreateCustomerIdentityParams{
		ID: xid.New().String(), CustomerID: bob, Provider: "google", ProviderSubject: "sub-bob",
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.w.DeleteSSOConnectionIdentities(f.ctx, "google"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.db.PgW.QueryRow(f.ctx, "select count(*) from customer_identities where customer_id = $1", bob).Scan(&n); err != nil || n != 1 {
		t.Fatalf("bob's identities = %d, %v; want 1", n, err)
	}
}

// waitForBlocked returns once a query waits on a lock, and fails if done fires first.
func (f *domainFixture) waitForBlocked(done <-chan error) {
	f.t.Helper()
	for start := time.Now(); ; {
		var waiting bool
		if err := f.db.PgW.QueryRow(f.ctx, "select exists (select 1 from pg_locks where not granted)").Scan(&waiting); err != nil {
			f.t.Fatal(err)
		}
		if waiting {
			return
		}
		select {
		case err := <-done:
			f.t.Fatalf("finished without waiting for the lock: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
		if time.Since(start) > 10*time.Second {
			f.t.Fatal("neither waited for the lock nor finished")
		}
	}
}

// A failed clear rolls back RemoveDomain's delete, so the claim stays for a retry.
func TestRemoveDomainRetriesAFailedClear(t *testing.T) {
	f, _, _ := newSSOConnectionFixture(t)
	orgID, _ := f.org("admin@acme.com")
	acme := f.verifiedDomain(orgID, "acme.com")
	f.mustConnection(orgID, orgs.SSOConnectionInput{DomainIDs: []string{acme.ID}})
	other, _ := f.org("admin@other.com")
	otherAcme := f.verifiedDomain(other, "acme.com")
	f.ssoSeen("acme.com")
	if _, err := f.db.PgW.Exec(f.ctx, `
create function fail_clear() returns trigger language plpgsql as $$
begin
  if new.sso_seen_at is null and old.sso_seen_at is not null then
    raise exception 'clear failed';
  end if;
  return new;
end $$;
create trigger fail_clear before update on org_domains for each row execute procedure fail_clear()`); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.RemoveDomain(f.ctx, orgID, acme.ID); err == nil {
		t.Fatal("RemoveDomain with a failing clear: want an error")
	}
	if _, err := f.db.PgW.Exec(f.ctx, "drop trigger fail_clear on org_domains"); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.RemoveDomain(f.ctx, orgID, acme.ID); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if _, err := f.svc.UpdateDomain(f.ctx, other, otherAcme.ID, true); !errors.Is(err, orgs.ErrDomainSSONotSeen) {
		t.Fatalf("Require SSO after the retry: err = %v, want ErrDomainSSONotSeen", err)
	}
}

// A clear that lands while Require SSO is being turned on reports why it failed.
func TestRequireSSORacingAClear(t *testing.T) {
	f := newDomainFixture(t)
	orgID, _ := f.org("admin@acme.com")
	acme := f.verifiedDomain(orgID, "acme.com")
	f.ssoSeen("acme.com")
	f.dns.during = func() {
		f.dns.during = nil
		if err := f.w.ClearOrgDomainsSSOSeen(f.ctx, []string{"acme.com"}); err != nil {
			t.Error(err)
		}
	}
	if _, err := f.svc.UpdateDomain(f.ctx, orgID, acme.ID, true); !errors.Is(err, orgs.ErrDomainSSONotSeen) {
		t.Fatalf("err = %v, want ErrDomainSSONotSeen", err)
	}
}

// An issuer change that commits while another save checks its issuer wins: that save
// kept the old issuer with no secret, and must not store the new issuer's secret under it.
func TestSSOConnectionSecretRecheckedUnderTheLock(t *testing.T) {
	f, cipher, _ := newSSOConnectionFixture(t)
	orgID, _ := f.org("admin@acme.com")
	acme := f.verifiedDomain(orgID, "acme.com")
	c := f.mustConnection(orgID, orgs.SSOConnectionInput{DomainIDs: []string{acme.ID}})
	changed := false
	f.svc.WithSSOConnections(cipher, func(context.Context, string) error {
		if !changed {
			changed = true
			f.mustConnection(orgID, orgs.SSOConnectionInput{ID: c.ID, IssuerURL: "https://new.okta.com", ClientSecret: "new-secret", DomainIDs: []string{acme.ID}})
		}
		return nil
	})

	if _, err := f.connection(orgID, orgs.SSOConnectionInput{ID: c.ID, Label: "Renamed", DomainIDs: []string{acme.ID}}); !errors.Is(err, orgs.ErrSSOConnectionSecretRequired) {
		t.Fatalf("err = %v, want ErrSSOConnectionSecretRequired", err)
	}
	conn, err := orgs.SSOConnectionForSignIn(f.ctx, f.w, cipher, c.ID)
	if err != nil || conn.IssuerURL != "https://new.okta.com" || conn.ClientSecret != "new-secret" {
		t.Fatalf("connection = %+v, %v; want the issuer change kept", conn, err)
	}
}

// A secret saved under another PUG_SSO_SECRET_KEY can't sign anyone in or be kept, so the
// admin enters it again.
func TestSSOConnectionSecretUnreadableAfterAKeyChange(t *testing.T) {
	f, _, _ := newSSOConnectionFixture(t)
	orgID, _ := f.org("admin@acme.com")
	acme := f.verifiedDomain(orgID, "acme.com")
	c := f.mustConnection(orgID, orgs.SSOConnectionInput{DomainIDs: []string{acme.ID}})
	rotated, err := secret.NewCipher(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("r", 32))))
	if err != nil {
		t.Fatal(err)
	}
	f.svc.WithSSOConnections(rotated, func(context.Context, string) error { return nil })

	if _, err := orgs.SSOConnectionForSignIn(f.ctx, f.w, rotated, c.ID); err == nil {
		t.Fatal("sign-in read a secret saved under another key")
	}
	if _, err := f.connection(orgID, orgs.SSOConnectionInput{ID: c.ID, Label: "Renamed", DomainIDs: []string{acme.ID}}); !errors.Is(err, orgs.ErrSSOConnectionSecretRequired) {
		t.Fatalf("blank secret: err = %v, want ErrSSOConnectionSecretRequired", err)
	}
	f.mustConnection(orgID, orgs.SSOConnectionInput{ID: c.ID, ClientSecret: "again", DomainIDs: []string{acme.ID}})
}
