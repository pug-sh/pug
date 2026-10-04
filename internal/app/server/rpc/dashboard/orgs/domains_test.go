package orgs_test

import (
	"context"
	"errors"
	"net"
	"testing"

	"connectrpc.com/connect"
	"github.com/rs/xid"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/protobuf/proto"

	orgshandler "github.com/pug-sh/pug/internal/app/server/rpc/dashboard/orgs"
	"github.com/pug-sh/pug/internal/apperr"
	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	orgsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/orgs/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
)

type txtRecords map[string][]string // keyed by rooted name

func (r txtRecords) LookupTXT(_ context.Context, name string) ([]string, error) {
	if v, ok := r[name]; ok {
		return v, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

type servfail struct{}

func (servfail) LookupTXT(_ context.Context, name string) ([]string, error) {
	return nil, &net.DNSError{Err: "server misbehaving", Name: name, IsTemporary: true}
}

func wantAppErr(t *testing.T, err error, code connect.Code, reason apperr.Reason) {
	t.Helper()
	ae, ok := errors.AsType[*apperr.Error](err)
	if !ok || ae.Code() != code || ae.Reason() != reason {
		t.Fatalf("err = %v, want %v / %s", err, code, reason)
	}
}

func seedCustomerWithEmail(t *testing.T, h orgsBackend, email string) dbread.Customer {
	t.Helper()
	id := xid.New().String()
	if _, err := h.write.CreateCustomer(h.ctx, dbwrite.CreateCustomerParams{
		ID: id, Email: email, DisplayName: "", PictureUri: "", PasswordHash: "x",
	}); err != nil {
		t.Fatalf("seed customer: %v", err)
	}
	c, err := h.read.GetCustomerByID(h.ctx, id)
	if err != nil {
		t.Fatalf("read customer: %v", err)
	}
	return c
}

func TestDomainHandlers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	h := setupOrgsBackend(t, nil)
	dns := txtRecords{}
	srv := orgshandler.NewServer(h.svc.WithTXTResolver(dns))
	admin := seedCustomerWithEmail(t, h, "admin@acme.com")
	org, err := h.svc.CreateOrgWithDefaults(h.ctx, admin.ID, "acme")
	if err != nil {
		t.Fatal(err)
	}
	ctx := ctxWithCustomer(h.ctx, admin)
	orgID := proto.String(org.ID)

	_, err = srv.AddDomain(ctx, connect.NewRequest(&orgsv1.AddDomainRequest{OrgId: orgID, Domain: proto.String("localhost")}))
	wantAppErr(t, err, connect.CodeInvalidArgument, apperr.ReasonDomainInvalid)
	_, err = srv.ListDomains(ctx, connect.NewRequest(&orgsv1.ListDomainsRequest{OrgId: proto.String(xid.New().String())}))
	wantAppErr(t, err, connect.CodeNotFound, apperr.ReasonOrgNotFound)
	_, err = srv.SetDomainSettings(ctx, connect.NewRequest(&orgsv1.SetDomainSettingsRequest{
		OrgId: orgID, AutoJoinRole: orgsv1.OrgRole(99).Enum(), MembersCanCreateOrgs: proto.Bool(true),
	}))
	wantAppErr(t, err, connect.CodeInvalidArgument, apperr.ReasonOrgUnsupportedRole)

	added, err := srv.AddDomain(ctx, connect.NewRequest(&orgsv1.AddDomainRequest{OrgId: orgID, Domain: proto.String("Acme.com")}))
	if err != nil {
		t.Fatalf("AddDomain: %v", err)
	}
	d := added.Msg.GetDomain()
	if d.GetDomain() != "acme.com" || d.GetStatus() != orgsv1.DomainStatus_DOMAIN_STATUS_PENDING || d.GetTxtRecordName() != "_pug-verification.acme.com" || d.GetVerifiedAt() != "" {
		t.Fatalf("added = %v", d)
	}

	_, err = srv.SetDomainSettings(ctx, connect.NewRequest(&orgsv1.SetDomainSettingsRequest{
		OrgId: orgID, AutoJoinRole: orgsv1.OrgRole_ORG_ROLE_VIEWER.Enum(), MembersCanCreateOrgs: proto.Bool(true),
	}))
	wantAppErr(t, err, connect.CodeFailedPrecondition, apperr.ReasonDomainNotVerified)

	h.svc.WithTXTResolver(servfail{})
	_, err = srv.VerifyDomain(ctx, connect.NewRequest(&orgsv1.VerifyDomainRequest{OrgId: orgID, DomainId: proto.String(d.GetId())}))
	wantAppErr(t, err, connect.CodeUnavailable, apperr.ReasonDomainLookupFailed)
	h.svc.WithTXTResolver(dns)

	_, err = srv.VerifyDomain(ctx, connect.NewRequest(&orgsv1.VerifyDomainRequest{OrgId: orgID, DomainId: proto.String(d.GetId())}))
	wantAppErr(t, err, connect.CodeFailedPrecondition, apperr.ReasonDomainVerificationFailed)
	ae, _ := errors.AsType[*apperr.Error](err)
	pf, ok := ae.Details()[0].(*errdetails.PreconditionFailure)
	if !ok || pf.GetViolations()[0].GetSubject() != "acme.com" {
		t.Fatalf("details = %v, want a precondition naming acme.com", ae.Details())
	}

	dns[d.GetTxtRecordName()+"."] = []string{d.GetTxtRecordValue()}
	verified, err := srv.VerifyDomain(ctx, connect.NewRequest(&orgsv1.VerifyDomainRequest{OrgId: orgID, DomainId: proto.String(d.GetId())}))
	if err != nil {
		t.Fatalf("VerifyDomain: %v", err)
	}
	if v := verified.Msg.GetDomain(); v.GetStatus() != orgsv1.DomainStatus_DOMAIN_STATUS_VERIFIED || v.GetVerificationMethod() != orgsv1.DomainVerificationMethod_DOMAIN_VERIFICATION_METHOD_DNS || v.GetVerifiedAt() == "" {
		t.Fatalf("verified = %v", v)
	}

	set, err := srv.SetDomainSettings(ctx, connect.NewRequest(&orgsv1.SetDomainSettingsRequest{
		OrgId: orgID, AutoJoinRole: orgsv1.OrgRole_ORG_ROLE_VIEWER.Enum(), MembersCanCreateOrgs: proto.Bool(true),
	}))
	if err != nil || set.Msg.GetSettings().GetAutoJoinRole() != orgsv1.OrgRole_ORG_ROLE_VIEWER {
		t.Fatalf("SetDomainSettings = %v, %v", set, err)
	}
	delete(dns, d.GetTxtRecordName()+".")
	_, err = srv.SetDomainSettings(ctx, connect.NewRequest(&orgsv1.SetDomainSettingsRequest{
		OrgId: orgID, AutoJoinRole: orgsv1.OrgRole_ORG_ROLE_VIEWER.Enum(), MembersCanCreateOrgs: proto.Bool(false),
	}))
	wantAppErr(t, err, connect.CodeFailedPrecondition, apperr.ReasonDomainVerificationFailed)

	list, err := srv.ListDomains(ctx, connect.NewRequest(&orgsv1.ListDomainsRequest{OrgId: orgID}))
	if err != nil {
		t.Fatalf("ListDomains: %v", err)
	}
	if s := list.Msg.GetSettings(); s.GetAutoJoinRole() != orgsv1.OrgRole_ORG_ROLE_VIEWER || !s.GetMembersCanCreateOrgs() || len(list.Msg.GetDomains()) != 1 {
		t.Fatalf("list = %v", list.Msg)
	}

	if _, err := srv.RemoveDomain(ctx, connect.NewRequest(&orgsv1.RemoveDomainRequest{OrgId: orgID, DomainId: proto.String(d.GetId())})); err != nil {
		t.Fatalf("RemoveDomain: %v", err)
	}
	_, err = srv.RemoveDomain(ctx, connect.NewRequest(&orgsv1.RemoveDomainRequest{OrgId: orgID, DomainId: proto.String(d.GetId())}))
	wantAppErr(t, err, connect.CodeNotFound, apperr.ReasonDomainNotFound)

	for i := range 10 {
		if _, err := h.svc.AddDomain(h.ctx, org.ID, "d"+string(rune('a'+i))+".acme.com"); err != nil {
			t.Fatal(err)
		}
	}
	_, err = srv.AddDomain(ctx, connect.NewRequest(&orgsv1.AddDomainRequest{OrgId: orgID, Domain: proto.String("acme.com")}))
	wantAppErr(t, err, connect.CodeFailedPrecondition, apperr.ReasonDomainLimitReached)
}

func TestOrgCreationRestrictionHandlers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	h := setupOrgsBackend(t, nil)
	srv := orgshandler.NewServer(h.svc)
	admin := seedCustomerWithEmail(t, h, "admin@acme.com")
	org, err := h.svc.CreateOrgWithDefaults(h.ctx, admin.ID, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.VerifyDomainByOperator(h.ctx, org.ID, "acme.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.SetDomainSettings(h.ctx, org.ID, coreorgs.DomainSettings{AutoJoinRole: coreorgs.RoleViewer, MembersCanCreateOrgs: false}); err != nil {
		t.Fatal(err)
	}
	bob := seedCustomerWithEmail(t, h, "bob@acme.com")
	if allowed, err := h.svc.OrgCreationAllowed(h.ctx, bob.ID, bob.Email); err != nil || allowed {
		t.Fatalf("OrgCreationAllowed = %v, %v; want restricted", allowed, err)
	}
	if _, err := h.pool.Exec(h.ctx, "insert into org_members (org_id, customer_id, role, joined_via_domain) values ($1, $2, 'ORG_ROLE_VIEWER', 'acme.com')", org.ID, bob.ID); err != nil {
		t.Fatal(err)
	}

	list, err := srv.List(ctxWithCustomer(h.ctx, bob), connect.NewRequest(&orgsv1.ListRequest{}))
	if err != nil || list.Msg.GetCanCreateOrg() {
		t.Fatalf("List = %v, %v; want can_create_org false", list, err)
	}
	adminList, err := srv.List(ctxWithCustomer(h.ctx, admin), connect.NewRequest(&orgsv1.ListRequest{}))
	if err != nil || !adminList.Msg.GetCanCreateOrg() {
		t.Fatalf("admin List = %v, %v; want can_create_org true", adminList, err)
	}

	_, err = srv.Create(ctxWithCustomer(h.ctx, bob), connect.NewRequest(&orgsv1.CreateRequest{DisplayName: proto.String("bob's")}))
	wantAppErr(t, err, connect.CodePermissionDenied, apperr.ReasonOrgCreationRestricted)

	members, err := srv.ListMembers(ctxWithCustomer(h.ctx, admin), connect.NewRequest(&orgsv1.ListMembersRequest{OrgId: proto.String(org.ID)}))
	if err != nil {
		t.Fatal(err)
	}
	via := map[string]string{}
	for _, m := range members.Msg.GetMembers() {
		via[m.GetEmail()] = m.GetJoinedViaDomain()
	}
	if via["bob@acme.com"] != "acme.com" || via["admin@acme.com"] != "" {
		t.Fatalf("joined_via_domain = %v", via)
	}
}

func TestRequireSSOHandlers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	h := setupOrgsBackend(t, nil)
	srv := orgshandler.NewServer(h.svc)
	admin := seedCustomerWithEmail(t, h, "admin@acme.com")
	org, err := h.svc.CreateOrgWithDefaults(h.ctx, admin.ID, "acme")
	if err != nil {
		t.Fatal(err)
	}
	pending, err := h.svc.AddDomain(h.ctx, org.ID, "acme.io")
	if err != nil {
		t.Fatal(err)
	}
	d, err := h.svc.VerifyDomainByOperator(h.ctx, org.ID, "acme.com")
	if err != nil {
		t.Fatal(err)
	}
	ctx := ctxWithCustomer(h.ctx, admin)
	update := func(domainID string) (*connect.Response[orgsv1.UpdateDomainResponse], error) {
		return srv.UpdateDomain(ctx, connect.NewRequest(&orgsv1.UpdateDomainRequest{
			OrgId: proto.String(org.ID), DomainId: proto.String(domainID), RequireSso: proto.Bool(true),
		}))
	}

	_, err = update(pending.ID)
	wantAppErr(t, err, connect.CodeFailedPrecondition, apperr.ReasonDomainNotVerified)
	_, err = update(d.ID)
	wantAppErr(t, err, connect.CodeFailedPrecondition, apperr.ReasonDomainSSONotSeen)
	_, err = update(xid.New().String())
	wantAppErr(t, err, connect.CodeNotFound, apperr.ReasonDomainNotFound)

	if err := coreorgs.MarkSSOSeenInTx(h.ctx, h.write, "acme.com"); err != nil {
		t.Fatal(err)
	}
	resp, err := update(d.ID)
	if err != nil {
		t.Fatalf("UpdateDomain: %v", err)
	}
	if got := resp.Msg.GetDomain(); !got.GetRequireSso() || !got.GetSsoSeen() {
		t.Fatalf("updated = %v", got)
	}
	list, err := srv.ListDomains(ctx, connect.NewRequest(&orgsv1.ListDomainsRequest{OrgId: proto.String(org.ID)}))
	if err != nil {
		t.Fatalf("ListDomains: %v", err)
	}
	if len(list.Msg.GetDomains()) != 2 {
		t.Fatalf("listed %d domains, want 2", len(list.Msg.GetDomains()))
	}
	for _, got := range list.Msg.GetDomains() {
		if got.GetRequireSso() != (got.GetDomain() == "acme.com") {
			t.Fatalf("listed = %v", got)
		}
	}
}
