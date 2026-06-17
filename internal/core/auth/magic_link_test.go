package auth_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	coreauth "github.com/pug-sh/pug/internal/core/auth"
	"github.com/pug-sh/pug/internal/core/emailaction"
	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	"github.com/pug-sh/pug/internal/deps/postgres"
	emailworkerv1 "github.com/pug-sh/pug/internal/gen/proto/workers/email/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
)

func TestSignInWithEmail_EmptyPasswordHashIsInvalidCredentials(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	ctx := context.Background()

	// A passwordless (magic-link) account: password_hash == "".
	if _, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID: "cust-nopw", Email: "nopw@example.com", DisplayName: "", PictureUri: "", PasswordHash: "",
	}); err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}

	svc := mustNewTestAuthService(t, db, &stubPublisher{})

	_, err := svc.SignInWithEmail(ctx, "nopw@example.com", "anything")
	if !errors.Is(err, coreauth.ErrInvalidCredentials) {
		t.Fatalf("err = %v, want ErrInvalidCredentials", err)
	}
}

func TestRequestMagicLink_IssuesTokenForKnownAndUnknownEmail(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	ctx := context.Background()

	if _, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID: "cust-known", Email: "known@example.com", DisplayName: "", PictureUri: "", PasswordHash: "hash",
	}); err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}
	pub := &stubPublisher{}
	svc := mustNewTestAuthService(t, db, pub)

	for _, email := range []string{"known@example.com", "stranger@example.com"} {
		if err := svc.RequestMagicLink(ctx, email); err != nil {
			t.Fatalf("RequestMagicLink(%s): %v", email, err)
		}
	}

	// Both the known and the unknown email get a magic-link email with a token.
	if len(pub.jobs) != 2 {
		t.Fatalf("expected 2 published magic-link jobs, got %d", len(pub.jobs))
	}
	for _, pj := range pub.jobs {
		ml, ok := pj.job.Payload.(*emailworkerv1.EmailJob_MagicLink)
		if !ok {
			t.Fatalf("published job is not a magic link: %T", pj.job.Payload)
		}
		if ml.MagicLink.GetToken() == "" {
			t.Fatal("magic link job missing token")
		}
	}
}

func TestCompleteMagicLink_NewEmailCreatesVerifiedAccountAndOrg(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	read := dbread.New(db.PgRO)
	ctx := context.Background()
	pub := &stubPublisher{}
	svc := mustNewTestAuthService(t, db, pub)

	if err := svc.RequestMagicLink(ctx, "fresh@example.com"); err != nil {
		t.Fatalf("RequestMagicLink: %v", err)
	}
	raw := lastMagicToken(t, pub)

	jwtTok, err := svc.CompleteMagicLink(ctx, raw, "")
	if err != nil {
		t.Fatalf("CompleteMagicLink: %v", err)
	}
	if jwtTok == "" {
		t.Fatal("expected a session JWT")
	}
	cust, err := read.GetCustomerByEmail(ctx, "fresh@example.com")
	if err != nil {
		t.Fatalf("GetCustomerByEmail: %v", err)
	}
	if !cust.EmailVerifiedAt.Valid {
		t.Fatal("expected email_verified_at set")
	}
	orgs, err := read.GetOrgsByCustomerID(ctx, cust.ID)
	if err != nil || len(orgs) == 0 {
		t.Fatalf("expected a default org, got %d (err=%v)", len(orgs), err)
	}

	// Single-use: a second completion fails.
	if _, err := svc.CompleteMagicLink(ctx, raw, ""); !errors.Is(err, coreauth.ErrInvalidToken) {
		t.Fatalf("second use err = %v, want ErrInvalidToken", err)
	}
}

// The browser timezone passed to CompleteMagicLink is stored as the auto-created
// default project's reporting_timezone; a malformed value coerces to "" (UTC) so
// signup never fails on it.
func TestCompleteMagicLink_CapturesBrowserTimezoneOnDefaultProject(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	read := dbread.New(db.PgRO)
	ctx := context.Background()

	defaultProjectTZ := func(t *testing.T, email, tz string) string {
		t.Helper()
		pub := &stubPublisher{}
		svc := mustNewTestAuthService(t, db, pub)
		if err := svc.RequestMagicLink(ctx, email); err != nil {
			t.Fatalf("RequestMagicLink: %v", err)
		}
		if _, err := svc.CompleteMagicLink(ctx, lastMagicToken(t, pub), tz); err != nil {
			t.Fatalf("CompleteMagicLink: %v", err)
		}
		cust, err := read.GetCustomerByEmail(ctx, email)
		if err != nil {
			t.Fatalf("GetCustomerByEmail: %v", err)
		}
		orgs, err := read.GetOrgsByCustomerID(ctx, cust.ID)
		if err != nil || len(orgs) == 0 {
			t.Fatalf("expected a default org, got %d (err=%v)", len(orgs), err)
		}
		projects, err := read.GetProjectsByOrgID(ctx, orgs[0].ID)
		if err != nil || len(projects) == 0 {
			t.Fatalf("expected a default project, got %d (err=%v)", len(projects), err)
		}
		return projects[0].ReportingTimezone
	}

	if got := defaultProjectTZ(t, "tz-valid@example.com", "Asia/Kolkata"); got != "Asia/Kolkata" {
		t.Errorf("reporting_timezone = %q, want Asia/Kolkata", got)
	}
	if got := defaultProjectTZ(t, "tz-bad@example.com", "Not/A/Zone"); got != "" {
		t.Errorf("malformed tz reporting_timezone = %q, want \"\" (UTC)", got)
	}
}

func TestCompleteMagicLink_InvalidToken(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	ctx := context.Background()
	svc := mustNewTestAuthService(t, db, &stubPublisher{})
	if _, err := svc.CompleteMagicLink(ctx, "no-such-token", ""); !errors.Is(err, coreauth.ErrInvalidToken) {
		t.Fatalf("err = %v, want ErrInvalidToken", err)
	}
}

func lastMagicToken(t *testing.T, pub *stubPublisher) string {
	t.Helper()
	if len(pub.jobs) == 0 {
		t.Fatal("no published jobs")
	}
	last := pub.jobs[len(pub.jobs)-1].job
	ml, ok := last.Payload.(*emailworkerv1.EmailJob_MagicLink)
	if !ok {
		t.Fatalf("last job is not a magic link: %T", last.Payload)
	}
	return ml.MagicLink.GetToken()
}

func TestCompleteMagicLink_InviteJoinsOrgWithRole(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	read := dbread.New(db.PgRO)
	ctx := context.Background()

	orgsSvc := coreorgs.NewService(db.PgRO, db.PgW, &stubPublisher{})
	inviter, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{ID: "cust-mlinv-inviter", Email: "mlinv-inviter@example.com", DisplayName: "Inviter", PasswordHash: "h", PictureUri: ""})
	if err != nil {
		t.Fatalf("CreateCustomer inviter: %v", err)
	}
	org, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: "org-mlinv", DisplayName: "ML Invite Org"})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{OrgID: org.ID, CustomerID: inviter.ID, Role: coreorgs.RoleAdmin.String()}); err != nil {
		t.Fatalf("CreateOrgMember: %v", err)
	}
	dispatch, err := orgsSvc.InviteMemberWithRole(ctx, org.ID, inviter.ID, "mlinv-invitee@example.com", coreorgs.RoleAdmin)
	if err != nil {
		t.Fatalf("InviteMemberWithRole: %v", err)
	}

	svc := mustNewTestAuthService(t, db, &stubPublisher{})
	jwtTok, err := svc.CompleteMagicLink(ctx, dispatch.RawToken, "")
	if err != nil {
		t.Fatalf("CompleteMagicLink: %v", err)
	}
	if jwtTok == "" {
		t.Fatal("expected JWT")
	}
	cust, err := read.GetCustomerByEmail(ctx, "mlinv-invitee@example.com")
	if err != nil {
		t.Fatalf("GetCustomerByEmail: %v", err)
	}
	orgsForCust, err := read.GetOrgsByCustomerID(ctx, cust.ID)
	if err != nil || len(orgsForCust) != 1 || orgsForCust[0].ID != org.ID {
		t.Fatalf("orgs = %+v err=%v, want exactly the invited org %s", orgsForCust, err, org.ID)
	}
	role, err := write.GetOrgMemberRole(ctx, dbwrite.GetOrgMemberRoleParams{OrgID: org.ID, CustomerID: cust.ID})
	if err != nil || role != coreorgs.RoleAdmin.String() {
		t.Fatalf("role = %q err=%v, want ADMIN", role, err)
	}
}

func TestCompleteMagicLink_InviteExistingAccountJoinsOrg(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	read := dbread.New(db.PgRO)
	ctx := context.Background()

	orgsSvc := coreorgs.NewService(db.PgRO, db.PgW, &stubPublisher{})
	inviter, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{ID: "cust-mlexist-inviter", Email: "mlexist-inviter@example.com", DisplayName: "Inviter", PasswordHash: "h", PictureUri: ""})
	if err != nil {
		t.Fatalf("CreateCustomer inviter: %v", err)
	}
	org, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: "org-mlexist", DisplayName: "ML Exist Org"})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{OrgID: org.ID, CustomerID: inviter.ID, Role: coreorgs.RoleAdmin.String()}); err != nil {
		t.Fatalf("CreateOrgMember: %v", err)
	}
	// Invitee already has an account (with a password).
	existing, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{ID: "cust-mlexist-invitee", Email: "mlexist-invitee@example.com", DisplayName: "Existing", PasswordHash: "h", PictureUri: ""})
	if err != nil {
		t.Fatalf("CreateCustomer invitee: %v", err)
	}
	dispatch, err := orgsSvc.InviteMemberWithRole(ctx, org.ID, inviter.ID, "mlexist-invitee@example.com", coreorgs.RoleMember)
	if err != nil {
		t.Fatalf("InviteMemberWithRole: %v", err)
	}

	svc := mustNewTestAuthService(t, db, &stubPublisher{})
	if _, err := svc.CompleteMagicLink(ctx, dispatch.RawToken, ""); err != nil {
		t.Fatalf("CompleteMagicLink: %v", err)
	}
	// The EXISTING customer joined the invited org; no new account, no default org.
	orgsForCust, err := read.GetOrgsByCustomerID(ctx, existing.ID)
	if err != nil || len(orgsForCust) != 1 || orgsForCust[0].ID != org.ID {
		t.Fatalf("orgs = %+v err=%v, want exactly the invited org for the existing account", orgsForCust, err)
	}
}

func TestCompleteMagicLink_ExpiredInviteTokenRejected(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	ctx := context.Background()

	orgsSvc := coreorgs.NewService(db.PgRO, db.PgW, &stubPublisher{})
	inviter, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{ID: "cust-mlexp-inviter", Email: "mlexp-inviter@example.com", DisplayName: "Inviter", PasswordHash: "h", PictureUri: ""})
	if err != nil {
		t.Fatalf("CreateCustomer inviter: %v", err)
	}
	org, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: "org-mlexp", DisplayName: "ML Exp Org"})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{OrgID: org.ID, CustomerID: inviter.ID, Role: coreorgs.RoleAdmin.String()}); err != nil {
		t.Fatalf("CreateOrgMember: %v", err)
	}
	dispatch, err := orgsSvc.InviteMemberWithRole(ctx, org.ID, inviter.ID, "mlexp-invitee@example.com", coreorgs.RoleMember)
	if err != nil {
		t.Fatalf("InviteMemberWithRole: %v", err)
	}
	// An EXPIRED magic-link token carrying the same invitation.
	if _, err := write.CreateEmailActionToken(ctx, dbwrite.CreateEmailActionTokenParams{
		ID:              "eat-mlexp",
		CustomerID:      postgres.NewOptionalText(""),
		Email:           "mlexp-invitee@example.com",
		Purpose:         emailaction.PurposeOrgInvite.String(),
		TokenHash:       hashToken("expired-invite-raw"),
		OrgInvitationID: postgres.NewOptionalText(dispatch.Invitation.ID),
		ExpiresAt:       postgres.NewTimestamptz(time.Now().Add(-time.Hour)),
	}); err != nil {
		t.Fatalf("CreateEmailActionToken: %v", err)
	}

	svc := mustNewTestAuthService(t, db, &stubPublisher{})
	if _, err := svc.CompleteMagicLink(ctx, "expired-invite-raw", ""); !errors.Is(err, coreauth.ErrInvalidToken) {
		t.Fatalf("err = %v, want ErrInvalidToken", err)
	}
}

// A pending invitation must survive the invitee requesting a plain login link
// for the same email. Invites and login links are distinct token purposes, so
// RequestMagicLink (which supersedes prior *login* links) must not consume the
// invite token — otherwise the invitee silently lands in a fresh default org
// instead of the inviting org.
func TestCompleteMagicLink_PlainRequestPreservesPendingInvite(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	read := dbread.New(db.PgRO)
	ctx := context.Background()

	orgsSvc := coreorgs.NewService(db.PgRO, db.PgW, &stubPublisher{})
	inviter, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{ID: "cust-pres-inviter", Email: "preserve-inviter@example.com", DisplayName: "Inviter", PasswordHash: "h", PictureUri: ""})
	if err != nil {
		t.Fatalf("CreateCustomer inviter: %v", err)
	}
	org, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: "org-preserve", DisplayName: "Preserve Org"})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{OrgID: org.ID, CustomerID: inviter.ID, Role: coreorgs.RoleAdmin.String()}); err != nil {
		t.Fatalf("CreateOrgMember: %v", err)
	}
	dispatch, err := orgsSvc.InviteMemberWithRole(ctx, org.ID, inviter.ID, "preserve-invitee@example.com", coreorgs.RoleMember)
	if err != nil {
		t.Fatalf("InviteMemberWithRole: %v", err)
	}

	svc := mustNewTestAuthService(t, db, &stubPublisher{})

	// Invitee requests a plain login link for the same email before clicking the invite.
	if err := svc.RequestMagicLink(ctx, "preserve-invitee@example.com"); err != nil {
		t.Fatalf("RequestMagicLink: %v", err)
	}

	// The invite token must still redeem and join the invited org.
	if _, err := svc.CompleteMagicLink(ctx, dispatch.RawToken, ""); err != nil {
		t.Fatalf("invite token should survive a plain magic-link request, got: %v", err)
	}
	cust, err := read.GetCustomerByEmail(ctx, "preserve-invitee@example.com")
	if err != nil {
		t.Fatalf("GetCustomerByEmail: %v", err)
	}
	orgsForCust, err := read.GetOrgsByCustomerID(ctx, cust.ID)
	if err != nil || len(orgsForCust) != 1 || orgsForCust[0].ID != org.ID {
		t.Fatalf("orgs = %+v err=%v, want exactly the invited org %s", orgsForCust, err, org.ID)
	}
}

// A magic link is single-use even under concurrent redemption: firing N
// completions of the same token (e.g. a double-click) must yield exactly one
// success, with the rest returning ErrInvalidToken — never a CodeInternal from a
// duplicate-customer race. The FOR UPDATE lock on the token row serializes them.
func TestCompleteMagicLink_ConcurrentRedemptionSerializes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	read := dbread.New(db.PgRO)
	ctx := context.Background()
	pub := &stubPublisher{}
	svc := mustNewTestAuthService(t, db, pub)

	if err := svc.RequestMagicLink(ctx, "concurrent@example.com"); err != nil {
		t.Fatalf("RequestMagicLink: %v", err)
	}
	raw := lastMagicToken(t, pub)

	const n = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := svc.CompleteMagicLink(ctx, raw, "")
			results[i] = err
		}()
	}
	close(start) // release all goroutines together to maximize overlap
	wg.Wait()

	successes := 0
	for _, err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, coreauth.ErrInvalidToken):
			// expected for the losers of the race
		default:
			t.Errorf("concurrent completion returned unexpected error: %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("want exactly 1 successful redemption, got %d", successes)
	}

	// Exactly one account + one default org, despite N concurrent attempts.
	cust, err := read.GetCustomerByEmail(ctx, "concurrent@example.com")
	if err != nil {
		t.Fatalf("GetCustomerByEmail: %v", err)
	}
	orgsForCust, err := read.GetOrgsByCustomerID(ctx, cust.ID)
	if err != nil || len(orgsForCust) != 1 {
		t.Fatalf("orgs = %d err=%v, want exactly 1", len(orgsForCust), err)
	}
}

// An existing account completing a plain (non-invite) magic link just signs in;
// it must NOT be provisioned a second default org. Guards the `createdNew` check
// in CompleteMagicLink's plain branch.
func TestCompleteMagicLink_ExistingAccountPlainLinkCreatesNoSecondOrg(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	read := dbread.New(db.PgRO)
	ctx := context.Background()

	if _, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{ID: "cust-existplain", Email: "existplain@example.com", DisplayName: "", PasswordHash: "h", PictureUri: ""}); err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}
	orgsSvc := coreorgs.NewService(db.PgRO, db.PgW, &stubPublisher{})
	if _, err := orgsSvc.CreateOrgWithDefaults(ctx, "cust-existplain", "existing-org"); err != nil {
		t.Fatalf("CreateOrgWithDefaults: %v", err)
	}

	pub := &stubPublisher{}
	svc := mustNewTestAuthService(t, db, pub)
	if err := svc.RequestMagicLink(ctx, "existplain@example.com"); err != nil {
		t.Fatalf("RequestMagicLink: %v", err)
	}
	if _, err := svc.CompleteMagicLink(ctx, lastMagicToken(t, pub), ""); err != nil {
		t.Fatalf("CompleteMagicLink: %v", err)
	}

	orgsForCust, err := read.GetOrgsByCustomerID(ctx, "cust-existplain")
	if err != nil {
		t.Fatalf("GetOrgsByCustomerID: %v", err)
	}
	if len(orgsForCust) != 1 {
		t.Fatalf("org count = %d, want 1 (existing account must not get a second default org)", len(orgsForCust))
	}
}
