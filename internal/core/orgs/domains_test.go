package orgs_test

import (
	"context"
	"errors"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/rs/xid"

	"github.com/pug-sh/pug/internal/core/orgs"
	orgsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/orgs/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
)

type fakeResolver struct {
	mu      sync.Mutex
	records map[string][]string // keyed by rooted name
	fail    error
	lookups int
	// during runs inside each lookup, as a call racing the one that looked up.
	during func()
}

func (f *fakeResolver) LookupTXT(_ context.Context, name string) ([]string, error) {
	if f.during != nil {
		f.during()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups++
	if f.fail != nil {
		return nil, f.fail
	}
	if r, ok := f.records[name]; ok {
		return r, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func (f *fakeResolver) publish(d orgs.Domain) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records[d.TXTRecordName()+"."] = append(f.records[d.TXTRecordName()+"."], d.TXTRecordValue())
}

func (f *fakeResolver) unpublish(d orgs.Domain) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records[d.TXTRecordName()+"."] = slices.DeleteFunc(f.records[d.TXTRecordName()+"."], func(v string) bool {
		return v == d.TXTRecordValue()
	})
}

type domainFixture struct {
	t   *testing.T
	ctx context.Context
	db  *testutil.TestPostgres
	w   *dbwrite.Queries
	svc *orgs.Service
	dns *fakeResolver
}

func newDomainFixture(t *testing.T) *domainFixture {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	dns := &fakeResolver{records: map[string][]string{}}
	return &domainFixture{
		t:   t,
		ctx: context.Background(),
		db:  db,
		w:   dbwrite.New(db.PgW),
		svc: orgs.NewService(db.PgRO, db.PgW, &stubPublisher{}).WithTXTResolver(dns),
		dns: dns,
	}
}

func (f *domainFixture) customer(email string) string {
	f.t.Helper()
	id := xid.New().String()
	if _, err := f.w.CreateCustomer(f.ctx, dbwrite.CreateCustomerParams{
		ID: id, Email: email, DisplayName: "", PictureUri: "", PasswordHash: "x",
	}); err != nil {
		f.t.Fatalf("create customer %s: %v", email, err)
	}
	return id
}

// org creates an org whose admin has adminEmail.
func (f *domainFixture) org(adminEmail string) (orgID, adminID string) {
	f.t.Helper()
	adminID = f.customer(adminEmail)
	org, err := f.svc.CreateOrgWithDefaults(f.ctx, adminID, adminEmail)
	if err != nil {
		f.t.Fatalf("create org: %v", err)
	}
	return org.ID, adminID
}

func (f *domainFixture) addDomain(orgID, domain string) orgs.Domain {
	f.t.Helper()
	d, err := f.svc.AddDomain(f.ctx, orgID, domain)
	if err != nil {
		f.t.Fatalf("AddDomain(%s): %v", domain, err)
	}
	return d
}

func (f *domainFixture) verifiedDomain(orgID, domain string) orgs.Domain {
	f.t.Helper()
	d := f.addDomain(orgID, domain)
	f.dns.publish(d)
	verified, err := f.svc.VerifyDomain(f.ctx, orgID, d.ID)
	if err != nil {
		f.t.Fatalf("VerifyDomain(%s): %v", domain, err)
	}
	return verified
}

func (f *domainFixture) settings(orgID string, role orgs.Role, canCreate bool) {
	f.t.Helper()
	if _, err := f.svc.SetDomainSettings(f.ctx, orgID, orgs.DomainSettings{AutoJoinRole: role, MembersCanCreateOrgs: canCreate}); err != nil {
		f.t.Fatalf("SetDomainSettings: %v", err)
	}
}

func (f *domainFixture) inTx(fn func(w *dbwrite.Queries) error) error {
	f.t.Helper()
	return pgx.BeginFunc(f.ctx, f.db.PgW, func(tx pgx.Tx) error {
		return fn(dbwrite.New(tx))
	})
}

func (f *domainFixture) autoJoin(customerID, email, domain string) []string {
	f.t.Helper()
	var joined []string
	if err := f.inTx(func(w *dbwrite.Queries) error {
		var err error
		joined, err = orgs.AutoJoinInTx(f.ctx, w, customerID, email, domain)
		return err
	}); err != nil {
		f.t.Fatalf("AutoJoinInTx: %v", err)
	}
	return joined
}

func (f *domainFixture) member(orgID, customerID string) (dbread.GetOrgMemberByOrgIDAndCustomerIDRow, bool) {
	f.t.Helper()
	row, err := dbread.New(f.db.PgW).GetOrgMemberByOrgIDAndCustomerID(f.ctx, dbread.GetOrgMemberByOrgIDAndCustomerIDParams{
		OrgID: orgID, CustomerID: customerID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return row, false
	}
	if err != nil {
		f.t.Fatalf("get member: %v", err)
	}
	return row, true
}

func wantVerificationError(t *testing.T, err error, domain string) {
	t.Helper()
	verr, ok := errors.AsType[*orgs.DomainVerificationError](err)
	if !ok || verr.Domain != domain {
		t.Fatalf("err = %v, want a verification error for %s", err, domain)
	}
}

func TestAddDomain(t *testing.T) {
	f := newDomainFixture(t)
	orgID, _ := f.org("admin@acme.com")

	d := f.addDomain(orgID, " Acme.COM. ")
	if d.Domain != "acme.com" || d.Verified() || len(d.VerificationToken) != 32 {
		t.Fatalf("domain = %+v", d)
	}
	if d.TXTRecordName() != "_pug-verification.acme.com" || d.TXTRecordValue() != "pug-verification="+d.VerificationToken {
		t.Fatalf("record = %s %s", d.TXTRecordName(), d.TXTRecordValue())
	}

	again := f.addDomain(orgID, "acme.com")
	if again.ID != d.ID || again.VerificationToken != d.VerificationToken {
		t.Fatalf("re-adding returned %+v, want the existing row %+v", again, d)
	}

	other, _ := f.org("admin@globex.com")
	if theirs := f.addDomain(other, "acme.com"); theirs.VerificationToken == d.VerificationToken {
		t.Fatal("a second org must get its own verification value")
	}

	if _, err := f.svc.AddDomain(f.ctx, orgID, "localhost"); !errors.Is(err, orgs.ErrDomainInvalid) {
		t.Fatalf("single-label err = %v, want ErrDomainInvalid", err)
	}
	if _, err := f.svc.AddDomain(f.ctx, xid.New().String(), "acme.com"); !errors.Is(err, orgs.ErrOrgNotFound) {
		t.Fatalf("unknown org err = %v, want ErrOrgNotFound", err)
	}

	for i := range 9 {
		f.addDomain(orgID, "d"+string(rune('a'+i))+".acme.com")
	}
	if _, err := f.svc.AddDomain(f.ctx, orgID, "one-too-many.com"); !errors.Is(err, orgs.ErrDomainLimitReached) {
		t.Fatalf("11th domain err = %v, want ErrDomainLimitReached", err)
	}
	if again := f.addDomain(orgID, "acme.com"); again.ID != d.ID {
		t.Fatal("an existing domain must still be returned at the limit")
	}
}

func TestVerifyDomain(t *testing.T) {
	f := newDomainFixture(t)
	orgID, _ := f.org("admin@acme.com")
	d := f.addDomain(orgID, "acme.com")

	_, err := f.svc.VerifyDomain(f.ctx, orgID, d.ID)
	wantVerificationError(t, err, "acme.com")

	f.dns.records[d.TXTRecordName()+"."] = []string{"pug-verification=someone-else", "v=spf1 -all"}
	_, err = f.svc.VerifyDomain(f.ctx, orgID, d.ID)
	wantVerificationError(t, err, "acme.com")

	f.dns.publish(d)
	f.dns.fail = &net.DNSError{Err: "server misbehaving", Name: d.TXTRecordName(), IsTemporary: true}
	if _, err := f.svc.VerifyDomain(f.ctx, orgID, d.ID); !errors.Is(err, orgs.ErrDNSUnavailable) {
		t.Fatalf("SERVFAIL err = %v, want ErrDNSUnavailable", err)
	}
	if _, domains, _ := f.svc.ListDomains(f.ctx, orgID); len(domains) != 1 || domains[0].Verified() {
		t.Fatalf("domains = %+v, want it still pending after a failed lookup", domains)
	}
	f.dns.fail = nil
	verified, err := f.svc.VerifyDomain(f.ctx, orgID, d.ID)
	if err != nil {
		t.Fatalf("VerifyDomain: %v", err)
	}
	if !verified.Verified() || verified.VerificationMethod != orgs.VerificationMethodDNS {
		t.Fatalf("domain = %+v, want verified by dns", verified)
	}

	f.dns.unpublish(d)
	if again, err := f.svc.VerifyDomain(f.ctx, orgID, d.ID); err != nil || !again.VerifiedAt.Equal(verified.VerifiedAt) {
		t.Fatalf("re-verify = %+v, %v; want the verified row unchanged", again, err)
	}

	other, _ := f.org("admin@globex.com")
	if _, err := f.svc.VerifyDomain(f.ctx, other, d.ID); !errors.Is(err, orgs.ErrDomainNotFound) {
		t.Fatalf("foreign org err = %v, want ErrDomainNotFound", err)
	}
}

func TestRemoveDomainKeepsMembers(t *testing.T) {
	f := newDomainFixture(t)
	orgID, _ := f.org("admin@acme.com")
	d := f.verifiedDomain(orgID, "acme.com")
	f.settings(orgID, orgs.RoleViewer, true)
	bob := f.customer("bob@acme.com")
	f.autoJoin(bob, "bob@acme.com", "acme.com")

	other, _ := f.org("admin@globex.com")
	if err := f.svc.RemoveDomain(f.ctx, other, d.ID); !errors.Is(err, orgs.ErrDomainNotFound) {
		t.Fatalf("foreign org err = %v, want ErrDomainNotFound", err)
	}
	if err := f.svc.RemoveDomain(f.ctx, orgID, d.ID); err != nil {
		t.Fatalf("RemoveDomain: %v", err)
	}
	if err := f.svc.RemoveDomain(f.ctx, orgID, d.ID); !errors.Is(err, orgs.ErrDomainNotFound) {
		t.Fatalf("second remove err = %v, want ErrDomainNotFound", err)
	}
	if _, ok := f.member(orgID, bob); !ok {
		t.Fatal("removing the domain must keep the members who joined through it")
	}
	if joined := f.autoJoin(f.customer("carol@acme.com"), "carol@acme.com", "acme.com"); len(joined) != 0 {
		t.Fatalf("auto-join through a removed domain joined %v", joined)
	}
}

func TestSetDomainSettingsNeedsAVerifiedDomain(t *testing.T) {
	f := newDomainFixture(t)
	orgID, _ := f.org("admin@acme.com")
	f.addDomain(orgID, "acme.com")

	for _, want := range []orgs.DomainSettings{
		{AutoJoinRole: orgs.RoleViewer, MembersCanCreateOrgs: true},
		{MembersCanCreateOrgs: false},
	} {
		if _, err := f.svc.SetDomainSettings(f.ctx, orgID, want); !errors.Is(err, orgs.ErrDomainNotVerified) {
			t.Fatalf("SetDomainSettings(%+v) err = %v, want ErrDomainNotVerified", want, err)
		}
	}
	got, err := f.svc.SetDomainSettings(f.ctx, orgID, orgs.DomainSettings{MembersCanCreateOrgs: true})
	if err != nil || got != (orgs.DomainSettings{MembersCanCreateOrgs: true}) {
		t.Fatalf("the defaults = %+v, %v; want them to always apply", got, err)
	}
	if _, err := f.svc.SetDomainSettings(f.ctx, xid.New().String(), orgs.DomainSettings{MembersCanCreateOrgs: true}); !errors.Is(err, orgs.ErrOrgNotFound) {
		t.Fatalf("unknown org err = %v, want ErrOrgNotFound", err)
	}
}

func TestSetDomainSettingsRechecksTheRecord(t *testing.T) {
	f := newDomainFixture(t)
	orgID, _ := f.org("admin@acme.com")
	d := f.verifiedDomain(orgID, "acme.com")

	f.dns.unpublish(d)
	for _, role := range []orgs.Role{orgs.RoleViewer, orgs.RoleMember} {
		_, err := f.svc.SetDomainSettings(f.ctx, orgID, orgs.DomainSettings{AutoJoinRole: role, MembersCanCreateOrgs: true})
		wantVerificationError(t, err, "acme.com")
	}
	f.dns.publish(d)

	f.settings(orgID, orgs.RoleViewer, true)
	// The record is live, so only the database's check can refuse this.
	if _, err := f.svc.SetDomainSettings(f.ctx, orgID, orgs.DomainSettings{AutoJoinRole: orgs.RoleAdmin, MembersCanCreateOrgs: true}); err == nil {
		t.Fatal("auto-join must never store the admin role")
	}
	settings, _, err := f.svc.ListDomains(f.ctx, orgID)
	if err != nil || settings != (orgs.DomainSettings{AutoJoinRole: orgs.RoleViewer, MembersCanCreateOrgs: true}) {
		t.Fatalf("settings = %+v, %v", settings, err)
	}

	f.dns.unpublish(d)
	for _, want := range []orgs.DomainSettings{
		{AutoJoinRole: orgs.RoleMember, MembersCanCreateOrgs: true},
		{AutoJoinRole: orgs.RoleViewer, MembersCanCreateOrgs: false},
	} {
		_, err := f.svc.SetDomainSettings(f.ctx, orgID, want)
		wantVerificationError(t, err, "acme.com")
	}
	if settings, _, _ := f.svc.ListDomains(f.ctx, orgID); settings.AutoJoinRole != orgs.RoleViewer {
		t.Fatalf("a refused change must leave the settings alone, got %+v", settings)
	}
	// Keeping the current settings or going back to the defaults needs no record.
	f.settings(orgID, orgs.RoleViewer, true)
	f.settings(orgID, "", true)

	// Nor does lowering the auto-join role.
	f.dns.publish(d)
	f.settings(orgID, orgs.RoleMember, true)
	f.dns.unpublish(d)
	f.settings(orgID, orgs.RoleViewer, true)

	// An operator-verified domain needs no record.
	if _, err := f.svc.VerifyDomainByOperator(f.ctx, orgID, "acme.com"); err != nil {
		t.Fatalf("VerifyDomainByOperator: %v", err)
	}
	f.settings(orgID, orgs.RoleMember, false)
}

// One good domain doesn't cover a stale DNS claim listed after it.
func TestSetDomainSettingsRechecksEveryDNSDomain(t *testing.T) {
	f := newDomainFixture(t)
	orgID, _ := f.org("admin@acme.com")
	if _, err := f.svc.VerifyDomainByOperator(f.ctx, orgID, "acme.io"); err != nil {
		t.Fatalf("VerifyDomainByOperator: %v", err)
	}
	f.verifiedDomain(orgID, "acme.com")
	f.dns.unpublish(f.verifiedDomain(orgID, "globex.com"))

	for _, want := range []orgs.DomainSettings{
		{AutoJoinRole: orgs.RoleViewer, MembersCanCreateOrgs: true},
		{MembersCanCreateOrgs: false},
	} {
		_, err := f.svc.SetDomainSettings(f.ctx, orgID, want)
		wantVerificationError(t, err, "globex.com")
	}
}

func TestAutoJoinAddsToEveryOrgThatVerifiedTheDomain(t *testing.T) {
	f := newDomainFixture(t)
	viewerOrg, _ := f.org("admin@acme.com")
	f.verifiedDomain(viewerOrg, "acme.com")
	f.settings(viewerOrg, orgs.RoleViewer, true)

	memberOrg, _ := f.org("admin2@acme.com")
	f.verifiedDomain(memberOrg, "acme.com")
	f.settings(memberOrg, orgs.RoleMember, true)

	offOrg, _ := f.org("admin3@acme.com")
	f.verifiedDomain(offOrg, "acme.com")

	pendingOrg, _ := f.org("admin@acme.io")
	f.verifiedDomain(pendingOrg, "acme.io")
	f.settings(pendingOrg, orgs.RoleMember, true)
	f.addDomain(pendingOrg, "acme.com")

	bob := f.customer("bob@acme.com")
	joined := f.autoJoin(bob, "bob@acme.com", "acme.com")
	slices.Sort(joined)
	want := []string{viewerOrg, memberOrg}
	slices.Sort(want)
	if !slices.Equal(joined, want) {
		t.Fatalf("joined = %v, want %v", joined, want)
	}
	for org, role := range map[string]orgs.Role{viewerOrg: orgs.RoleViewer, memberOrg: orgs.RoleMember} {
		m, ok := f.member(org, bob)
		if !ok || m.Role != role.String() || m.JoinedViaDomain.String != "acme.com" {
			t.Fatalf("membership in %s = %+v (ok=%v), want %s via acme.com", org, m, ok, role)
		}
	}
	for _, org := range []string{offOrg, pendingOrg} {
		if _, ok := f.member(org, bob); ok {
			t.Fatalf("joined %s, which has auto-join off or only a pending claim", org)
		}
	}

	if again := f.autoJoin(bob, "bob@acme.com", "acme.com"); len(again) != 0 {
		t.Fatalf("second run joined %v, want nothing", again)
	}
	if joined := f.autoJoin(bob, "bob@acme.com", ""); len(joined) != 0 {
		t.Fatalf("no proven domain joined %v", joined)
	}
	if joined := f.autoJoin(f.customer("dan@gmail.com"), "dan@gmail.com", "acme.com"); len(joined) != 0 {
		t.Fatalf("an account on another domain joined %v", joined)
	}

	// Turning auto-join off stops new joins and keeps the people who joined.
	f.settings(viewerOrg, "", true)
	if joined := f.autoJoin(f.customer("carol@acme.com"), "carol@acme.com", "acme.com"); !slices.Equal(joined, []string{memberOrg}) {
		t.Fatalf("after turning auto-join off joined = %v, want only %s", joined, memberOrg)
	}
	if _, ok := f.member(viewerOrg, bob); !ok {
		t.Fatal("turning auto-join off removed a member who had joined")
	}
}

func TestAutoJoinLeavesAPendingInviteToDecideTheRole(t *testing.T) {
	f := newDomainFixture(t)
	orgID, adminID := f.org("admin@acme.com")
	f.verifiedDomain(orgID, "acme.com")
	f.settings(orgID, orgs.RoleViewer, true)

	dispatch, err := f.svc.InviteMemberWithRole(f.ctx, orgID, adminID, "Carol@acme.com", orgs.RoleAdmin)
	if err != nil {
		t.Fatalf("InviteMemberWithRole: %v", err)
	}
	carol := f.customer("carol@acme.com")
	if joined := f.autoJoin(carol, "carol@acme.com", "acme.com"); len(joined) != 0 {
		t.Fatalf("auto-join pre-empted a pending invite: joined %v", joined)
	}
	if err := f.inTx(func(w *dbwrite.Queries) error {
		_, err := orgs.ApplyInviteAcceptanceInTx(f.ctx, w, dispatch.Invitation.ID, carol)
		return err
	}); err != nil {
		t.Fatalf("accept invite: %v", err)
	}
	if m, _ := f.member(orgID, carol); m.Role != orgs.RoleAdmin.String() || m.JoinedViaDomain.Valid {
		t.Fatalf("membership = %+v, want the invite's admin role", m)
	}

	// An expired invite no longer holds the org back.
	dave := f.customer("dave@acme.com")
	expired, err := f.svc.InviteMemberWithRole(f.ctx, orgID, adminID, "dave@acme.com", orgs.RoleMember)
	if err != nil {
		t.Fatalf("InviteMemberWithRole: %v", err)
	}
	if _, err := f.db.PgW.Exec(f.ctx, "update org_invitations set expires_at = now() - interval '1 hour' where id = $1", expired.Invitation.ID); err != nil {
		t.Fatalf("expire invite: %v", err)
	}
	// Only dave's own invite, in that org, holds an org back.
	if _, err := f.svc.InviteMemberWithRole(f.ctx, orgID, adminID, "erin@acme.com", orgs.RoleMember); err != nil {
		t.Fatalf("InviteMemberWithRole: %v", err)
	}
	otherOrg, otherAdmin := f.org("admin2@acme.com")
	f.verifiedDomain(otherOrg, "acme.com")
	f.settings(otherOrg, orgs.RoleViewer, true)
	if _, err := f.svc.InviteMemberWithRole(f.ctx, otherOrg, otherAdmin, "dave@acme.com", orgs.RoleMember); err != nil {
		t.Fatalf("InviteMemberWithRole: %v", err)
	}
	if joined := f.autoJoin(dave, "dave@acme.com", "acme.com"); !slices.Equal(joined, []string{orgID}) {
		t.Fatalf("joined = %v, want only %s", joined, orgID)
	}
}

// An existing membership must not abort the caller's transaction.
func TestApplyInviteAcceptanceInTxAlreadyMemberKeepsTheTransaction(t *testing.T) {
	f := newDomainFixture(t)
	orgID, adminID := f.org("admin@acme.com")
	dispatch, err := f.svc.InviteMemberWithRole(f.ctx, orgID, adminID, "bob@acme.com", orgs.RoleAdmin)
	if err != nil {
		t.Fatalf("InviteMemberWithRole: %v", err)
	}
	bob := f.customer("bob@acme.com")
	mustAddMember(t, f.ctx, f.w, orgID, bob, orgs.RoleViewer.String())

	err = f.inTx(func(w *dbwrite.Queries) error {
		if _, err := orgs.ApplyInviteAcceptanceInTx(f.ctx, w, dispatch.Invitation.ID, bob); !errors.Is(err, orgs.ErrAlreadyMember) {
			t.Fatalf("apply err = %v, want ErrAlreadyMember", err)
		}
		_, err := w.GetOrgMemberRole(f.ctx, dbwrite.GetOrgMemberRoleParams{OrgID: orgID, CustomerID: bob})
		return err
	})
	if err != nil {
		t.Fatalf("the transaction was aborted: %v", err)
	}
	if m, _ := f.member(orgID, bob); m.Role != orgs.RoleViewer.String() {
		t.Fatalf("role = %s, want the existing viewer role kept", m.Role)
	}
	inv, err := f.w.GetOrgInvitationByIDForUpdate(f.ctx, dispatch.Invitation.ID)
	if err != nil || inv.Status != orgsv1.InvitationStatus_INVITATION_STATUS_ACCEPTED.String() {
		t.Fatalf("invitation status = %q, %v; want ACCEPTED", inv.Status, err)
	}
}

func TestOrgCreationRestriction(t *testing.T) {
	f := newDomainFixture(t)
	openOrg, openAdmin := f.org("admin@acme.com")
	f.verifiedDomain(openOrg, "acme.com")
	strictOrg, strictAdmin := f.org("admin2@acme.com")
	f.verifiedDomain(strictOrg, "acme.com")

	bob := f.customer("bob@acme.com")
	mustAddMember(t, f.ctx, f.w, openOrg, bob, orgs.RoleMember.String())
	bobsOrg, err := f.svc.CreateOrgWithDefaults(f.ctx, bob, "bob's")
	if err != nil {
		t.Fatal(err)
	}
	f.addDomain(bobsOrg.ID, "acme.com")
	jane := f.customer("jane@gmail.com")
	mustAddMember(t, f.ctx, f.w, strictOrg, jane, orgs.RoleMember.String())

	f.settings(strictOrg, "", false)

	allowed := func(id, email string) bool {
		t.Helper()
		ok, err := f.svc.OrgCreationAllowed(f.ctx, id, email)
		if err != nil {
			t.Fatalf("OrgCreationAllowed: %v", err)
		}
		return ok
	}
	if allowed(bob, "bob@acme.com") {
		t.Fatal("one org turning creation off must restrict the domain (strictest wins), even for an admin of an org whose claim is pending")
	}
	if _, err := f.svc.CreateOrgWithDefaults(f.ctx, bob, "bob's org"); !errors.Is(err, orgs.ErrOrgCreationRestricted) {
		t.Fatalf("Create err = %v, want ErrOrgCreationRestricted", err)
	}
	for name, c := range map[string][2]string{
		"an admin of the org that allows it":  {openAdmin, "admin@acme.com"},
		"an admin of the org that forbids it": {strictAdmin, "admin2@acme.com"},
		"a member on another domain":          {jane, "jane@gmail.com"},
	} {
		if !allowed(c[0], c[1]) {
			t.Fatalf("%s must still be allowed", name)
		}
	}
	if _, err := f.svc.CreateOrgWithDefaults(f.ctx, jane, "jane's org"); err != nil {
		t.Fatalf("Create for a gmail member: %v", err)
	}

	// A pending claim restricts nothing.
	elsewhere, _ := f.org("admin@acme.io")
	f.verifiedDomain(elsewhere, "acme.io")
	f.settings(elsewhere, "", false)
	f.addDomain(elsewhere, "acme.org")
	if !allowed(f.customer("carol@acme.org"), "carol@acme.org") {
		t.Fatal("an org's pending domain must not restrict it")
	}

	f.settings(strictOrg, "", true)
	if !allowed(bob, "bob@acme.com") {
		t.Fatal("turning creation back on in every org must lift the restriction")
	}
}

func TestListDomainsShowsAStricterOrgOnlyToAVerifiedClaim(t *testing.T) {
	f := newDomainFixture(t)
	strictOrg, _ := f.org("admin@acme.com")
	f.verifiedDomain(strictOrg, "acme.com")
	verifiedOrg, _ := f.org("admin2@acme.com")
	f.verifiedDomain(verifiedOrg, "acme.com")
	pendingOrg, _ := f.org("admin@globex.com")
	f.addDomain(pendingOrg, "acme.com")
	f.settings(strictOrg, "", false)

	for org, want := range map[string]bool{verifiedOrg: true, pendingOrg: false, strictOrg: false} {
		_, domains, err := f.svc.ListDomains(f.ctx, org)
		if err != nil || len(domains) != 1 {
			t.Fatalf("ListDomains = %v, %v", domains, err)
		}
		if domains[0].OrgCreationRestrictedElsewhere != want {
			t.Fatalf("org %s restricted elsewhere = %v, want %v", org, domains[0].OrgCreationRestrictedElsewhere, want)
		}
	}
}

func TestOperatorVerifyAndRelease(t *testing.T) {
	f := newDomainFixture(t)
	orgID, _ := f.org("admin@acme.com")

	d, err := f.svc.VerifyDomainByOperator(f.ctx, orgID, "ACME.com")
	if err != nil {
		t.Fatalf("VerifyDomainByOperator: %v", err)
	}
	if d.Domain != "acme.com" || !d.Verified() || d.VerificationMethod != orgs.VerificationMethodOperator {
		t.Fatalf("domain = %+v, want verified by the operator", d)
	}
	if f.dns.lookups != 0 {
		t.Fatalf("operator verification looked up DNS %d times", f.dns.lookups)
	}

	dnsVerified := f.verifiedDomain(orgID, "acme.io")
	again, err := f.svc.VerifyDomainByOperator(f.ctx, orgID, "acme.io")
	if err != nil || again.ID != dnsVerified.ID || again.VerificationMethod != orgs.VerificationMethodOperator || !again.VerifiedAt.Equal(dnsVerified.VerifiedAt) {
		t.Fatalf("operator over dns = %+v, %v; want the same row, now the operator's", again, err)
	}

	other, _ := f.org("admin@globex.com")
	f.addDomain(other, "acme.com")
	claims, err := f.svc.DomainClaims(f.ctx, "acme.com")
	if err != nil || len(claims) != 2 || claims[0].OrgID != orgID || !claims[0].VerifiedAt.Valid || claims[1].VerifiedAt.Valid {
		t.Fatalf("claims = %+v, %v; want the verified claim first", claims, err)
	}

	if err := f.svc.ReleaseDomain(f.ctx, orgID, "acme.com"); err != nil {
		t.Fatalf("ReleaseDomain: %v", err)
	}
	if claims, err := f.svc.DomainClaims(f.ctx, "acme.com"); err != nil || len(claims) != 1 || claims[0].OrgID != other {
		t.Fatalf("claims after release = %+v, %v; want only the other org's", claims, err)
	}
	if err := f.svc.ReleaseDomain(f.ctx, orgID, "acme.com"); !errors.Is(err, orgs.ErrDomainNotFound) {
		t.Fatalf("second release err = %v, want ErrDomainNotFound", err)
	}
	if _, err := f.svc.VerifyDomainByOperator(f.ctx, xid.New().String(), "acme.com"); !errors.Is(err, orgs.ErrOrgNotFound) {
		t.Fatalf("unknown org err = %v, want ErrOrgNotFound", err)
	}
	if err := f.svc.ReleaseDomain(f.ctx, xid.New().String(), "acme.com"); !errors.Is(err, orgs.ErrOrgNotFound) {
		t.Fatalf("release for an unknown org err = %v, want ErrOrgNotFound", err)
	}

	for name, call := range map[string]func() error{
		"VerifyDomainByOperator": func() error { _, err := f.svc.VerifyDomainByOperator(f.ctx, orgID, "localhost"); return err },
		"ReleaseDomain":          func() error { return f.svc.ReleaseDomain(f.ctx, orgID, "localhost") },
		"UnenforceDomain":        func() error { _, err := f.svc.UnenforceDomain(f.ctx, "localhost"); return err },
		"DomainClaims":           func() error { _, err := f.svc.DomainClaims(f.ctx, "localhost"); return err },
	} {
		if err := call(); !errors.Is(err, orgs.ErrDomainInvalid) {
			t.Fatalf("%s err = %v, want ErrDomainInvalid", name, err)
		}
	}
}

// A call landing during the DNS lookup wins, and the check reports what it left.
func TestDNSCheckRacingAnotherCall(t *testing.T) {
	f := newDomainFixture(t)
	orgID, _ := f.org("admin@acme.com")
	operator := f.addDomain(orgID, "acme.com")
	f.dns.publish(operator)
	removed := f.addDomain(orgID, "acme.io")
	f.dns.publish(removed)
	requireSSO := f.verifiedDomain(orgID, "acme.org")
	f.ssoSeen("acme.org")

	f.dns.during = func() {
		if _, err := f.svc.VerifyDomainByOperator(f.ctx, orgID, "acme.com"); err != nil {
			t.Errorf("VerifyDomainByOperator: %v", err)
		}
	}
	if got, err := f.svc.VerifyDomain(f.ctx, orgID, operator.ID); err != nil || got.VerificationMethod != orgs.VerificationMethodOperator {
		t.Fatalf("VerifyDomain = %+v, %v; want the operator's verification", got, err)
	}

	remove := func(d orgs.Domain) func() {
		return func() {
			if err := f.svc.RemoveDomain(f.ctx, orgID, d.ID); err != nil {
				t.Errorf("RemoveDomain(%s): %v", d.Domain, err)
			}
		}
	}
	f.dns.during = remove(removed)
	if _, err := f.svc.VerifyDomain(f.ctx, orgID, removed.ID); !errors.Is(err, orgs.ErrDomainNotFound) {
		t.Fatalf("VerifyDomain err = %v, want ErrDomainNotFound", err)
	}
	f.dns.during = remove(requireSSO)
	if _, err := f.svc.UpdateDomain(f.ctx, orgID, requireSSO.ID, true); !errors.Is(err, orgs.ErrDomainNotFound) {
		t.Fatalf("UpdateDomain err = %v, want ErrDomainNotFound", err)
	}
}

func TestMarkSSOSeenInTxMarksOnlyVerifiedClaims(t *testing.T) {
	f := newDomainFixture(t)
	verifiedOrg, _ := f.org("admin@acme.com")
	f.verifiedDomain(verifiedOrg, "acme.com")
	pendingOrg, _ := f.org("admin@globex.com")
	f.addDomain(pendingOrg, "acme.com")

	if err := f.inTx(func(w *dbwrite.Queries) error { return orgs.MarkSSOSeenInTx(f.ctx, w, "acme.com") }); err != nil {
		t.Fatalf("MarkSSOSeenInTx: %v", err)
	}
	claims, err := f.svc.DomainClaims(f.ctx, "acme.com")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range claims {
		if c.SsoSeenAt.Valid != c.VerifiedAt.Valid {
			t.Fatalf("claim of %s: sso seen = %v, verified = %v", strings.TrimSpace(c.OrgID), c.SsoSeenAt.Valid, c.VerifiedAt.Valid)
		}
	}
}

func (f *domainFixture) ssoSeen(domain string) {
	f.t.Helper()
	if err := f.inTx(func(w *dbwrite.Queries) error { return orgs.MarkSSOSeenInTx(f.ctx, w, domain) }); err != nil {
		f.t.Fatalf("MarkSSOSeenInTx: %v", err)
	}
}

func (f *domainFixture) requireSSO(orgID string, d orgs.Domain, on bool) {
	f.t.Helper()
	if _, err := f.svc.UpdateDomain(f.ctx, orgID, d.ID, on); err != nil {
		f.t.Fatalf("UpdateDomain(%s, %v): %v", d.Domain, on, err)
	}
}

func wantSSORequired(t *testing.T, err error, domain string) {
	t.Helper()
	ssoErr, ok := errors.AsType[*orgs.SSORequiredError](err)
	if !ok || ssoErr.Domain != domain {
		t.Fatalf("err = %v, want SSORequiredError for %s", err, domain)
	}
}

func TestRequireSSONeedsAVerifiedDomainThatSSOHasProven(t *testing.T) {
	f := newDomainFixture(t)
	orgID, _ := f.org("admin@acme.com")
	d := f.addDomain(orgID, "acme.com")
	f.dns.publish(d)

	if _, err := f.svc.UpdateDomain(f.ctx, orgID, d.ID, true); !errors.Is(err, orgs.ErrDomainNotVerified) {
		t.Fatalf("pending domain err = %v, want ErrDomainNotVerified", err)
	}
	if _, err := f.svc.VerifyDomain(f.ctx, orgID, d.ID); err != nil {
		t.Fatalf("VerifyDomain: %v", err)
	}
	if _, err := f.svc.UpdateDomain(f.ctx, orgID, d.ID, true); !errors.Is(err, orgs.ErrDomainSSONotSeen) {
		t.Fatalf("unproven domain err = %v, want ErrDomainSSONotSeen", err)
	}
	if _, err := f.svc.UpdateDomain(f.ctx, xid.New().String(), d.ID, true); !errors.Is(err, orgs.ErrDomainNotFound) {
		t.Fatalf("another org's domain err = %v, want ErrDomainNotFound", err)
	}

	f.ssoSeen("acme.com")
	got, err := f.svc.UpdateDomain(f.ctx, orgID, d.ID, true)
	if err != nil || !got.RequireSSO || !got.SSOSeen {
		t.Fatalf("UpdateDomain = %+v, %v; want Require SSO on", got, err)
	}
	_, domains, err := f.svc.ListDomains(f.ctx, orgID)
	if err != nil || len(domains) != 1 || !domains[0].RequireSSO || !domains[0].SSOSeen {
		t.Fatalf("ListDomains = %+v, %v", domains, err)
	}
}

func TestRequireSSORechecksTheRecord(t *testing.T) {
	f := newDomainFixture(t)
	orgID, _ := f.org("admin@acme.com")
	d := f.verifiedDomain(orgID, "acme.com")
	f.ssoSeen("acme.com")

	f.dns.fail = &net.DNSError{Err: "server misbehaving", IsTemporary: true}
	if _, err := f.svc.UpdateDomain(f.ctx, orgID, d.ID, true); !errors.Is(err, orgs.ErrDNSUnavailable) {
		t.Fatalf("err = %v, want ErrDNSUnavailable", err)
	}
	f.dns.fail = nil
	f.dns.unpublish(d)
	_, err := f.svc.UpdateDomain(f.ctx, orgID, d.ID, true)
	wantVerificationError(t, err, "acme.com")
	if err := orgs.CheckSignInInTx(f.ctx, f.w, "bob@acme.com", ""); err != nil {
		t.Fatalf("a refused change must leave Require SSO off, got %v", err)
	}

	f.dns.publish(d)
	f.requireSSO(orgID, d, true)
	// Turning it off needs no record.
	f.dns.unpublish(d)
	f.requireSSO(orgID, d, false)

	op, err := f.svc.VerifyDomainByOperator(f.ctx, orgID, "acme.io")
	if err != nil {
		t.Fatalf("VerifyDomainByOperator: %v", err)
	}
	f.ssoSeen("acme.io")
	lookups := f.dns.lookups
	f.requireSSO(orgID, op, true)
	if f.dns.lookups != lookups {
		t.Fatal("an operator-verified domain looked up DNS")
	}
}

func TestRequireSSOStrictestWins(t *testing.T) {
	f := newDomainFixture(t)
	strictOrg, _ := f.org("admin@acme.com")
	d := f.verifiedDomain(strictOrg, "acme.com")
	laxOrg, _ := f.org("admin2@acme.com")
	laxD := f.verifiedDomain(laxOrg, "acme.com")
	pendingOrg, _ := f.org("admin@globex.com")
	f.addDomain(pendingOrg, "acme.com")
	f.ssoSeen("acme.com")
	f.requireSSO(strictOrg, d, true)

	wantSSORequired(t, orgs.CheckSignInInTx(f.ctx, f.w, "Bob@ACME.com", ""), "acme.com")
	wantSSORequired(t, orgs.CheckSignInInTx(f.ctx, f.w, "bob@acme.com", "acme.io"), "acme.com")
	for _, tc := range []struct{ email, proven string }{
		{"bob@acme.com", "acme.com"},
		{"bob@globex.com", ""},
		{"bob@eng.acme.com", ""},
	} {
		if err := orgs.CheckSignInInTx(f.ctx, f.w, tc.email, tc.proven); err != nil {
			t.Fatalf("CheckSignInInTx(%s, %q) = %v, want nil", tc.email, tc.proven, err)
		}
	}

	for org, want := range map[string]bool{laxOrg: true, pendingOrg: false, strictOrg: false} {
		_, domains, err := f.svc.ListDomains(f.ctx, org)
		if err != nil || len(domains) != 1 {
			t.Fatalf("ListDomains = %v, %v", domains, err)
		}
		if domains[0].SSORequiredElsewhere != want {
			t.Fatalf("org %s: required elsewhere = %v, want %v", org, domains[0].SSORequiredElsewhere, want)
		}
	}

	f.requireSSO(laxOrg, laxD, true)
	n, err := f.svc.UnenforceDomain(f.ctx, "ACME.com")
	if err != nil || n != 2 {
		t.Fatalf("UnenforceDomain = %d, %v; want 2 claims changed", n, err)
	}
	if err := orgs.CheckSignInInTx(f.ctx, f.w, "bob@acme.com", ""); err != nil {
		t.Fatalf("after unenforce: %v", err)
	}
	if n, err := f.svc.UnenforceDomain(f.ctx, "acme.com"); err != nil || n != 0 {
		t.Fatalf("UnenforceDomain again = %d, %v; want 0, nil", n, err)
	}
	if _, err := f.svc.UnenforceDomain(f.ctx, "acme.co"); !errors.Is(err, orgs.ErrDomainNotFound) {
		t.Fatalf("UnenforceDomain(unclaimed) err = %v, want ErrDomainNotFound", err)
	}
}
