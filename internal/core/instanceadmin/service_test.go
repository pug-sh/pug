package instanceadmin_test

import (
	"context"
	"errors"
	"testing"

	coreauth "github.com/pug-sh/pug/internal/core/auth"
	"github.com/pug-sh/pug/internal/core/instance"
	"github.com/pug-sh/pug/internal/core/instanceadmin"
	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	emailworkerv1 "github.com/pug-sh/pug/internal/gen/proto/workers/email/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
	"github.com/rs/xid"
	"google.golang.org/protobuf/proto"
)

type recordingPublisher struct{ data []byte }

func (p *recordingPublisher) Publish(_ context.Context, _ string, data []byte) error {
	p.data = data
	return nil
}

func TestInventoryAndLastAdminGuard(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Docker Desktop")
	}
	db := testutil.SetupPostgres(t)
	ctx := context.Background()
	policy, err := instance.ParsePolicy("managed", "admin@example.com,backup@example.com")
	if err != nil {
		t.Fatal(err)
	}
	w := dbwrite.New(db.PgW)
	admin, err := w.CreateCustomer(ctx, dbwrite.CreateCustomerParams{ID: xid.New().String(), Email: "admin@example.com", DisplayName: "", PasswordHash: "", PictureUri: ""})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.MarkCustomerEmailVerified(ctx, admin.ID); err != nil {
		t.Fatal(err)
	}
	user, err := w.CreateCustomer(ctx, dbwrite.CreateCustomerParams{ID: xid.New().String(), Email: "user@example.com", DisplayName: "", PasswordHash: "", PictureUri: ""})
	if err != nil {
		t.Fatal(err)
	}
	org, err := w.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: xid.New().String(), DisplayName: "Engineering"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{OrgID: org.ID, CustomerID: user.ID, Role: "ORG_ROLE_ADMIN"}); err != nil {
		t.Fatal(err)
	}
	svc := instanceadmin.NewService(db.PgRO, db.PgW, policy, nil)
	users, next, err := svc.ListUsers(ctx, instanceadmin.UserFilters{Search: "user@example.com"}, 10, "")
	if err != nil || next != "" || len(users) != 1 || len(users[0].Memberships) != 1 {
		t.Fatalf("users=%+v next=%q err=%v", users, next, err)
	}
	orgs, _, err := svc.ListOrganizations(ctx, "Engineering", 10, "")
	if err != nil || len(orgs) != 1 || orgs[0].MemberCount != 1 || len(orgs[0].AdminEmails) != 1 {
		t.Fatalf("orgs=%+v err=%v", orgs, err)
	}
	if err := svc.SetUserDisabled(ctx, admin.ID, admin.ID, true); !errors.Is(err, instanceadmin.ErrLastAdmin) {
		t.Fatalf("last admin disable: %v", err)
	}
	openPolicy, err := instance.ParsePolicy("open", "admin@example.com,backup@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := instanceadmin.NewService(db.PgRO, db.PgW, openPolicy, nil).SetUserDisabled(ctx, admin.ID, admin.ID, true); !errors.Is(err, instanceadmin.ErrLastAdmin) {
		t.Fatalf("last admin disable in open mode: %v", err)
	}
	if err := svc.SetUserDisabled(ctx, admin.ID, user.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateCustomer(ctx, dbwrite.CreateCustomerParams{ID: "00000000000000000000", Email: "contains-" + user.ID + "@example.com", DisplayName: "", PasswordHash: "", PictureUri: ""}); err != nil {
		t.Fatal(err)
	}
	disabledUser, err := svc.GetUser(ctx, user.ID)
	if err != nil || disabledUser.ID != user.ID || !disabledUser.Disabled || len(disabledUser.Memberships) != 1 {
		t.Fatalf("disabled user=%+v err=%v", disabledUser, err)
	}
	orgs, _, err = svc.ListOrganizations(ctx, org.ID, 10, "")
	if err != nil || len(orgs) != 1 || !orgs[0].NeedsAdmin {
		t.Fatalf("organization should flag disabled sole admin: %+v, %v", orgs, err)
	}
	var auditCount int
	if err := db.PgRO.QueryRow(ctx, `select count(*) from instance_audit where target_id=$1 and action='user.disabled'`, user.ID).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatalf("audit count=%d err=%v", auditCount, err)
	}
}

func TestListUsersFiltersBeforePagination(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Docker Desktop")
	}
	db := testutil.SetupPostgres(t)
	ctx := context.Background()
	w := dbwrite.New(db.PgW)
	org, err := w.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: xid.New().String(), DisplayName: "Engineering"})
	if err != nil {
		t.Fatal(err)
	}
	otherOrg, err := w.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: xid.New().String(), DisplayName: "Support"})
	if err != nil {
		t.Fatal(err)
	}
	createUser := func(email string, verified bool) string {
		t.Helper()
		user, err := w.CreateCustomer(ctx, dbwrite.CreateCustomerParams{ID: xid.New().String(), Email: email, DisplayName: "", PasswordHash: "", PictureUri: ""})
		if err != nil {
			t.Fatal(err)
		}
		if verified {
			if _, err := w.MarkCustomerEmailVerified(ctx, user.ID); err != nil {
				t.Fatal(err)
			}
		}
		return user.ID
	}
	verifiedID := createUser("verified@example.com", true)
	unverifiedID := createUser("unverified@example.com", false)
	otherID := createUser("other@example.com", true)
	for _, userID := range []string{verifiedID, unverifiedID} {
		if _, err := w.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{OrgID: org.ID, CustomerID: userID, Role: "ORG_ROLE_MEMBER"}); err != nil {
			t.Fatal(err)
		}
	}
	// A user may belong to multiple organizations; the organization filter must not duplicate rows.
	for _, userID := range []string{unverifiedID, otherID} {
		if _, err := w.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{OrgID: otherOrg.ID, CustomerID: userID, Role: "ORG_ROLE_MEMBER"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.PgW.Exec(ctx, `update customers set disabled_at=now() where id=$1`, unverifiedID); err != nil {
		t.Fatal(err)
	}
	svc := instanceadmin.NewService(db.PgRO, db.PgW, instance.Policy{}, nil)
	filters := instanceadmin.UserFilters{OrgID: org.ID}
	first, next, err := svc.ListUsers(ctx, filters, 1, "")
	if err != nil || len(first) != 1 || next == "" {
		t.Fatalf("first page=%+v next=%q err=%v", first, next, err)
	}
	second, final, err := svc.ListUsers(ctx, filters, 1, next)
	if err != nil || len(second) != 1 || final != "" || first[0].ID == second[0].ID {
		t.Fatalf("second page=%+v next=%q err=%v", second, final, err)
	}
	trueValue, falseValue := true, false
	cases := []struct {
		name    string
		filters instanceadmin.UserFilters
		wantID  string
	}{
		{"verified", instanceadmin.UserFilters{OrgID: org.ID, Verified: &trueValue}, verifiedID},
		{"unverified", instanceadmin.UserFilters{OrgID: org.ID, Verified: &falseValue}, unverifiedID},
		{"enabled", instanceadmin.UserFilters{OrgID: org.ID, Enabled: &trueValue}, verifiedID},
		{"disabled", instanceadmin.UserFilters{OrgID: org.ID, Enabled: &falseValue}, unverifiedID},
		{"combined", instanceadmin.UserFilters{OrgID: org.ID, Verified: &falseValue, Enabled: &falseValue}, unverifiedID},
		{"other organization", instanceadmin.UserFilters{OrgID: otherOrg.ID, Verified: &trueValue}, otherID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			users, next, err := svc.ListUsers(ctx, tc.filters, 1, "")
			if err != nil || len(users) != 1 || users[0].ID != tc.wantID || next != "" {
				t.Fatalf("users=%+v next=%q err=%v", users, next, err)
			}
		})
	}
}

func TestProvisionOrganizationKeepsOperatorOutsideMembership(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Docker Desktop")
	}
	db := testutil.SetupPostgres(t)
	ctx := context.Background()
	policy, err := instance.ParsePolicy("managed", "admin@example.com")
	if err != nil {
		t.Fatal(err)
	}
	w := dbwrite.New(db.PgW)
	admin, err := w.CreateCustomer(ctx, dbwrite.CreateCustomerParams{ID: xid.New().String(), Email: "admin@example.com", DisplayName: "", PasswordHash: "", PictureUri: ""})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.MarkCustomerEmailVerified(ctx, admin.ID); err != nil {
		t.Fatal(err)
	}
	publisher := &recordingPublisher{}
	orgs := coreorgs.NewService(db.PgRO, db.PgW, publisher)
	svc := instanceadmin.NewService(db.PgRO, db.PgW, policy, orgs)
	orgID, err := svc.ProvisionOrganization(ctx, admin.ID, "Acme", "first@example.com")
	if err != nil {
		t.Fatal(err)
	}
	detail, err := svc.GetOrganization(ctx, orgID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Organization.Name != "Acme" || detail.Organization.MemberCount != 0 || !detail.Organization.NeedsAdmin || len(detail.Projects) != 1 || len(detail.Invitations) != 1 || detail.Invitations[0].Email != "first@example.com" {
		t.Fatalf("provisioned organization: %+v", detail)
	}
	var auditCount int
	if err := db.PgRO.QueryRow(ctx, `select count(*) from instance_audit where target_id=$1 and action in ('organization.provisioned','organization.member_invited')`, orgID).Scan(&auditCount); err != nil || auditCount != 2 {
		t.Fatalf("audit count=%d err=%v", auditCount, err)
	}
	if err := db.PgRO.QueryRow(ctx, `select count(*) from instance_audit where target_id=$1 and action='organization.member_invited' and details->>'status'='completed'`, orgID).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatalf("completed invitation audit count=%d err=%v", auditCount, err)
	}
	job := &emailworkerv1.EmailJob{}
	if err := proto.Unmarshal(publisher.data, job); err != nil {
		t.Fatal(err)
	}
	inviteToken := job.GetOrgMemberInvite().GetToken()
	if inviteToken == "" {
		t.Fatal("initial admin invitation was not queued")
	}
	authSvc, err := coreauth.NewServiceForTest(ctx, db.PgRO, db.PgW, []byte("jwt-secret"), publisher, policy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authSvc.CompleteMagicLink(ctx, inviteToken, ""); err != nil {
		t.Fatalf("accept initial admin invitation: %v", err)
	}
	newAdmin, err := dbread.New(db.PgRO).GetCustomerByEmail(ctx, "first@example.com")
	if err != nil {
		t.Fatal(err)
	}
	membership, err := orgs.GetMember(ctx, orgID, newAdmin.ID)
	if err != nil || membership.Role != "ORG_ROLE_ADMIN" {
		t.Fatalf("initial admin membership: %+v, %v", membership, err)
	}
	operatorMemberships, err := dbread.New(db.PgRO).GetOrgsByCustomerID(ctx, admin.ID)
	if err != nil || len(operatorMemberships) != 0 {
		t.Fatalf("operator gained org membership: %+v, %v", operatorMemberships, err)
	}
}

func TestOrganizationDetailPagesEachCollection(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Docker Desktop")
	}
	db := testutil.SetupPostgres(t)
	ctx := context.Background()
	w := dbwrite.New(db.PgW)
	org, err := w.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: xid.New().String(), DisplayName: "Paged"})
	if err != nil {
		t.Fatal(err)
	}
	for i, email := range []string{"first@example.com", "second@example.com"} {
		user, err := w.CreateCustomer(ctx, dbwrite.CreateCustomerParams{ID: xid.New().String(), Email: email, DisplayName: "", PasswordHash: "", PictureUri: ""})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{OrgID: org.ID, CustomerID: user.ID, Role: "ORG_ROLE_MEMBER"}); err != nil {
			t.Fatal(err)
		}
		if _, err := db.PgW.Exec(ctx, `insert into projects(id,org_id,display_name) values($1,$2,$3)`, xid.New().String(), org.ID, email); err != nil {
			t.Fatal(err)
		}
		if _, err := db.PgW.Exec(ctx, `insert into org_invitations(id,org_id,email,expires_at,token) values($1,$2,$3,now()+interval '7 days',$4)`, xid.New().String(), org.ID, "invite"+string(rune('0'+i))+"@example.com", xid.New().String()+"abcdefghijkl"); err != nil {
			t.Fatal(err)
		}
	}
	svc := instanceadmin.NewService(db.PgRO, db.PgW, instance.Policy{}, nil)
	first, err := svc.GetOrganization(ctx, org.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Projects) != 1 || len(first.Members) != 1 || len(first.Invitations) != 1 || first.NextProjectPageToken == "" || first.NextMemberPageToken == "" || first.NextInvitationPageToken == "" {
		t.Fatalf("first page: %+v", first)
	}
	second, err := svc.GetOrganization(ctx, org.ID, 1, first.NextProjectPageToken, first.NextMemberPageToken, first.NextInvitationPageToken)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Projects) != 1 || len(second.Members) != 1 || len(second.Invitations) != 1 || second.NextProjectPageToken != "" || second.NextMemberPageToken != "" || second.NextInvitationPageToken != "" || second.Projects[0].ID == first.Projects[0].ID || second.Members[0].ID == first.Members[0].ID || second.Invitations[0].ID == first.Invitations[0].ID {
		t.Fatalf("second page: %+v", second)
	}
	if _, err := svc.GetOrganization(ctx, org.ID, 1, "invalid"); !errors.Is(err, instanceadmin.ErrInvalidPageToken) {
		t.Fatalf("invalid page token: %v", err)
	}
}
