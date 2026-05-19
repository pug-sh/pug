package orgemailproviders_test

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/authn"
	"connectrpc.com/connect"

	"github.com/pug-sh/pug/internal/app/server/rpc"
	"github.com/pug-sh/pug/internal/app/server/rpc/dashboard/orgemailproviders"
	coreemail "github.com/pug-sh/pug/internal/core/email"
	"github.com/pug-sh/pug/internal/core/email/secret"
	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	orgemailprovidersv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/orgemailproviders/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
)

func setupCipher(t *testing.T) *secret.Cipher {
	t.Helper()
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i)
	}
	c, err := secret.NewCipher(base64.StdEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	return c
}

// strPtr returns a pointer to s for proto optional-string assignments.
func strPtr(s string) *string { return &s }

// uint32Ptr returns a pointer to v for proto optional-uint32 assignments.
func uint32Ptr(v uint32) *uint32 { return &v }

// boolPtr returns a pointer to b for proto optional-bool assignments.
func boolPtr(b bool) *bool { return &b }

func TestSetGetRoundTripRedactsSecret(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	db := testutil.SetupPostgres(t)
	rds := testutil.SetupRedis(t)
	ctx := context.Background()

	write := dbwrite.New(db.PgW)
	read := dbread.New(db.PgRO)

	customer, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID: "cust-admin-1", Email: "admin@acme.com", DisplayName: "Admin", PasswordHash: "h", PictureUri: "",
	})
	if err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}
	org, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: "org-admin-1", DisplayName: "Acme"})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID: org.ID, CustomerID: customer.ID, Role: "ORG_ROLE_ADMIN",
	}); err != nil {
		t.Fatalf("CreateOrgMember: %v", err)
	}

	orgs := coreorgs.NewService(db.PgRO, db.PgW, nil)
	cipher := setupCipher(t)
	repo := coreemail.NewOrgProviderRepo(read, rds.Client)
	srv := orgemailproviders.NewServer(orgs, read, write, cipher, repo, nil)

	ctxWithPrincipal := authn.SetInfo(ctx, &rpc.Principal{
		AuthType: rpc.AuthTypeJWT,
		Customer: &dbread.Customer{ID: customer.ID},
	})

	_, err = srv.Set(ctxWithPrincipal, connect.NewRequest(&orgemailprovidersv1.SetRequest{
		OrgId:       strPtr(org.ID),
		FromAddress: strPtr("ops@acme.com"),
		ReplyTo:     strPtr("support@acme.com"),
		Config: &orgemailprovidersv1.SetRequest_Resend{
			Resend: &orgemailprovidersv1.ResendConfig{ApiKey: strPtr("sk_test_abcdef1234567890")},
		},
	}))
	if err != nil {
		t.Fatalf("Set: %v", err)
	}

	got, err := srv.Get(ctxWithPrincipal, connect.NewRequest(&orgemailprovidersv1.GetRequest{OrgId: strPtr(org.ID)}))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Msg.GetFromAddress() != "ops@acme.com" {
		t.Fatalf("from_address: got %q", got.Msg.GetFromAddress())
	}
	if got.Msg.GetReplyTo() != "support@acme.com" {
		t.Fatalf("reply_to: got %q", got.Msg.GetReplyTo())
	}
	if got.Msg.GetKind() != orgemailprovidersv1.OrgEmailProviderKind_ORG_EMAIL_PROVIDER_KIND_RESEND {
		t.Fatalf("kind: got %v", got.Msg.GetKind())
	}
	redacted := got.Msg.GetRedactedSecret()
	if strings.Contains(redacted, "abcdef") {
		t.Fatalf("redaction leaked secret middle: %q", redacted)
	}
	if !strings.HasPrefix(redacted, "sk_test_") || !strings.HasSuffix(redacted, "7890") {
		t.Fatalf("expected prefix+suffix redaction, got %q", redacted)
	}
}

func TestSetRequiresAdmin(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	db := testutil.SetupPostgres(t)
	rds := testutil.SetupRedis(t)
	ctx := context.Background()

	write := dbwrite.New(db.PgW)
	read := dbread.New(db.PgRO)
	customer, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID: "cust-member-1", Email: "member@acme.com", DisplayName: "Member", PasswordHash: "h", PictureUri: "",
	})
	if err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}
	org, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: "org-member-1", DisplayName: "Acme"})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID: org.ID, CustomerID: customer.ID, Role: "ORG_ROLE_MEMBER",
	}); err != nil {
		t.Fatalf("CreateOrgMember: %v", err)
	}

	orgs := coreorgs.NewService(db.PgRO, db.PgW, nil)
	cipher := setupCipher(t)
	repo := coreemail.NewOrgProviderRepo(read, rds.Client)
	srv := orgemailproviders.NewServer(orgs, read, write, cipher, repo, nil)

	ctxWithPrincipal := authn.SetInfo(ctx, &rpc.Principal{
		AuthType: rpc.AuthTypeJWT,
		Customer: &dbread.Customer{ID: customer.ID},
	})
	_, err = srv.Set(ctxWithPrincipal, connect.NewRequest(&orgemailprovidersv1.SetRequest{
		OrgId:       strPtr(org.ID),
		FromAddress: strPtr("x@acme.com"),
		Config: &orgemailprovidersv1.SetRequest_Resend{
			Resend: &orgemailprovidersv1.ResendConfig{ApiKey: strPtr("sk_test_abcdef1234567890")},
		},
	}))
	if err == nil {
		t.Fatal("expected admin error")
	}
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("expected PermissionDenied, got %v", err)
	}
}

// TestSetInvalidatesCache asserts that Set deletes any prior cache entry so
// the next provider lookup re-reads the row from Postgres. The repo caches
// the *previous* value under "email:org_provider:<org_id>"; if Set forgot to
// invalidate, the worker would keep dispatching with the stale ciphertext.
func TestSetInvalidatesCache(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	db := testutil.SetupPostgres(t)
	rds := testutil.SetupRedis(t)
	ctx := context.Background()

	write := dbwrite.New(db.PgW)
	read := dbread.New(db.PgRO)

	customer, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID: "cust-cache-1", Email: "cache@acme.com", DisplayName: "Admin", PasswordHash: "h", PictureUri: "",
	})
	if err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}
	org, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: "org-cache-1", DisplayName: "Acme"})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID: org.ID, CustomerID: customer.ID, Role: "ORG_ROLE_ADMIN",
	}); err != nil {
		t.Fatalf("CreateOrgMember: %v", err)
	}

	orgs := coreorgs.NewService(db.PgRO, db.PgW, nil)
	cipher := setupCipher(t)
	repo := coreemail.NewOrgProviderRepo(read, rds.Client)
	srv := orgemailproviders.NewServer(orgs, read, write, cipher, repo, nil)

	ctxWithPrincipal := authn.SetInfo(ctx, &rpc.Principal{
		AuthType: rpc.AuthTypeJWT,
		Customer: &dbread.Customer{ID: customer.ID},
	})

	// First Set + populate cache via repo.Get.
	if _, err := srv.Set(ctxWithPrincipal, connect.NewRequest(&orgemailprovidersv1.SetRequest{
		OrgId:       strPtr(org.ID),
		FromAddress: strPtr("ops@acme.com"),
		Config: &orgemailprovidersv1.SetRequest_Resend{
			Resend: &orgemailprovidersv1.ResendConfig{ApiKey: strPtr("sk_test_aaaaaaaaaaaaaaaa")},
		},
	})); err != nil {
		t.Fatalf("first Set: %v", err)
	}
	first, err := repo.Get(ctx, org.ID)
	if err != nil {
		t.Fatalf("repo.Get (first): %v", err)
	}
	if !first.Present {
		t.Fatal("expected first lookup to find provider")
	}
	firstCiphertext := append([]byte(nil), first.SecretCiphertext...)

	// Second Set rotates the secret; cache entry from the first lookup must
	// not survive the upsert.
	if _, err := srv.Set(ctxWithPrincipal, connect.NewRequest(&orgemailprovidersv1.SetRequest{
		OrgId:       strPtr(org.ID),
		FromAddress: strPtr("ops@acme.com"),
		Config: &orgemailprovidersv1.SetRequest_Resend{
			Resend: &orgemailprovidersv1.ResendConfig{ApiKey: strPtr("sk_test_bbbbbbbbbbbbbbbb")},
		},
	})); err != nil {
		t.Fatalf("second Set: %v", err)
	}

	second, err := repo.Get(ctx, org.ID)
	if err != nil {
		t.Fatalf("repo.Get (second): %v", err)
	}
	if !second.Present {
		t.Fatal("expected second lookup to find provider")
	}
	if string(second.SecretCiphertext) == string(firstCiphertext) {
		t.Fatal("cache was not invalidated: second lookup returned stale ciphertext")
	}
}

// TestSetGetRoundTripSMTP exercises the SMTP branch end-to-end and checks the
// password is replaced with *** in the redacted display string.
func TestSetGetRoundTripSMTP(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	db := testutil.SetupPostgres(t)
	rds := testutil.SetupRedis(t)
	ctx := context.Background()

	write := dbwrite.New(db.PgW)
	read := dbread.New(db.PgRO)

	customer, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID: "cust-smtp-1", Email: "smtp@acme.com", DisplayName: "Admin", PasswordHash: "h", PictureUri: "",
	})
	if err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}
	org, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: "org-smtp-1", DisplayName: "Acme"})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID: org.ID, CustomerID: customer.ID, Role: "ORG_ROLE_ADMIN",
	}); err != nil {
		t.Fatalf("CreateOrgMember: %v", err)
	}

	orgs := coreorgs.NewService(db.PgRO, db.PgW, nil)
	cipher := setupCipher(t)
	repo := coreemail.NewOrgProviderRepo(read, rds.Client)
	srv := orgemailproviders.NewServer(orgs, read, write, cipher, repo, nil)

	ctxWithPrincipal := authn.SetInfo(ctx, &rpc.Principal{
		AuthType: rpc.AuthTypeJWT,
		Customer: &dbread.Customer{ID: customer.ID},
	})
	if _, err := srv.Set(ctxWithPrincipal, connect.NewRequest(&orgemailprovidersv1.SetRequest{
		OrgId:       strPtr(org.ID),
		FromAddress: strPtr("ops@acme.com"),
		Config: &orgemailprovidersv1.SetRequest_Smtp{
			Smtp: &orgemailprovidersv1.SMTPConfig{
				Host:     strPtr("smtp.example.com"),
				Port:     uint32Ptr(587),
				Username: strPtr("postmaster"),
				Password: strPtr("super-secret-pw"),
				UseTls:   boolPtr(true),
			},
		},
	})); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got, err := srv.Get(ctxWithPrincipal, connect.NewRequest(&orgemailprovidersv1.GetRequest{OrgId: strPtr(org.ID)}))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Msg.GetKind() != orgemailprovidersv1.OrgEmailProviderKind_ORG_EMAIL_PROVIDER_KIND_SMTP {
		t.Fatalf("kind: got %v", got.Msg.GetKind())
	}
	redacted := got.Msg.GetRedactedSecret()
	if strings.Contains(redacted, "super-secret-pw") {
		t.Fatalf("redaction leaked smtp password: %q", redacted)
	}
	wantPrefix := "smtp://postmaster:***@smtp.example.com:587"
	if redacted != wantPrefix {
		t.Fatalf("unexpected redaction: got %q want %q", redacted, wantPrefix)
	}
}

// TestRemoveRequiresAdminAndInvalidates verifies non-admins can't remove the
// row, and that a successful remove makes a subsequent lookup return absent.
func TestRemoveRequiresAdminAndInvalidates(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	db := testutil.SetupPostgres(t)
	rds := testutil.SetupRedis(t)
	ctx := context.Background()

	write := dbwrite.New(db.PgW)
	read := dbread.New(db.PgRO)

	adminCust, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID: "cust-rem-admin", Email: "rmadmin@acme.com", DisplayName: "Admin", PasswordHash: "h", PictureUri: "",
	})
	if err != nil {
		t.Fatalf("CreateCustomer admin: %v", err)
	}
	memberCust, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID: "cust-rem-mem", Email: "rmmem@acme.com", DisplayName: "Member", PasswordHash: "h", PictureUri: "",
	})
	if err != nil {
		t.Fatalf("CreateCustomer member: %v", err)
	}
	org, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: "org-rem-1", DisplayName: "Acme"})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID: org.ID, CustomerID: adminCust.ID, Role: "ORG_ROLE_ADMIN",
	}); err != nil {
		t.Fatalf("CreateOrgMember admin: %v", err)
	}
	if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID: org.ID, CustomerID: memberCust.ID, Role: "ORG_ROLE_MEMBER",
	}); err != nil {
		t.Fatalf("CreateOrgMember member: %v", err)
	}

	orgs := coreorgs.NewService(db.PgRO, db.PgW, nil)
	cipher := setupCipher(t)
	repo := coreemail.NewOrgProviderRepo(read, rds.Client)
	srv := orgemailproviders.NewServer(orgs, read, write, cipher, repo, nil)

	adminCtx := authn.SetInfo(ctx, &rpc.Principal{
		AuthType: rpc.AuthTypeJWT,
		Customer: &dbread.Customer{ID: adminCust.ID},
	})
	memberCtx := authn.SetInfo(ctx, &rpc.Principal{
		AuthType: rpc.AuthTypeJWT,
		Customer: &dbread.Customer{ID: memberCust.ID},
	})

	if _, err := srv.Set(adminCtx, connect.NewRequest(&orgemailprovidersv1.SetRequest{
		OrgId:       strPtr(org.ID),
		FromAddress: strPtr("ops@acme.com"),
		Config: &orgemailprovidersv1.SetRequest_Resend{
			Resend: &orgemailprovidersv1.ResendConfig{ApiKey: strPtr("sk_test_abcdef1234567890")},
		},
	})); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Member cannot remove.
	if _, err := srv.Remove(memberCtx, connect.NewRequest(&orgemailprovidersv1.RemoveRequest{OrgId: strPtr(org.ID)})); err == nil {
		t.Fatal("expected admin error on Remove for member")
	} else if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("expected PermissionDenied, got %v", err)
	}

	// Admin can remove; cache lookup should then return absent.
	if _, err := srv.Remove(adminCtx, connect.NewRequest(&orgemailprovidersv1.RemoveRequest{OrgId: strPtr(org.ID)})); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	entry, err := repo.Get(ctx, org.ID)
	if err != nil {
		t.Fatalf("repo.Get after Remove: %v", err)
	}
	if entry.Present {
		t.Fatal("expected provider absent after Remove")
	}
}

// TestSendTestSurfacesProviderError asserts that when the mailer's resolver
// (or downstream provider) returns an error, SendTest returns Success=false
// with the underlying message in ErrorMessage instead of an RPC error. The
// admin who triggered the test send needs to see the provider error string
// (smtp connect failure, bad credentials, etc.) to fix their config.
func TestSendTestSurfacesProviderError(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	db := testutil.SetupPostgres(t)
	rds := testutil.SetupRedis(t)
	ctx := context.Background()

	write := dbwrite.New(db.PgW)
	read := dbread.New(db.PgRO)

	customer, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID: "cust-test-1", Email: "test-admin@acme.com", DisplayName: "Admin", PasswordHash: "h", PictureUri: "",
	})
	if err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}
	org, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: "org-test-1", DisplayName: "Acme"})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID: org.ID, CustomerID: customer.ID, Role: "ORG_ROLE_ADMIN",
	}); err != nil {
		t.Fatalf("CreateOrgMember: %v", err)
	}

	orgs := coreorgs.NewService(db.PgRO, db.PgW, nil)
	cipher := setupCipher(t)
	repo := coreemail.NewOrgProviderRepo(read, rds.Client)

	failingResolver := &alwaysFailingResolver{err: errors.New("smtp connect: connection refused")}
	mailer, err := coreemail.NewServiceWithResolver(coreemail.Config{
		DashboardBaseURL: "https://dashboard.example",
		From:             "noreply@example.com",
	}, failingResolver)
	if err != nil {
		t.Fatalf("NewServiceWithResolver: %v", err)
	}

	srv := orgemailproviders.NewServer(orgs, read, write, cipher, repo, mailer)
	ctxWithPrincipal := authn.SetInfo(ctx, &rpc.Principal{AuthType: rpc.AuthTypeJWT, Customer: &dbread.Customer{ID: customer.ID}})

	resp, err := srv.SendTest(ctxWithPrincipal, connect.NewRequest(&orgemailprovidersv1.SendTestRequest{
		OrgId: strPtr(org.ID), Recipient: strPtr("qa@acme.com"),
	}))
	if err != nil {
		t.Fatalf("SendTest: %v", err)
	}
	if resp.Msg.GetSuccess() {
		t.Fatal("expected Success=false")
	}
	if !strings.Contains(resp.Msg.GetErrorMessage(), "connection refused") {
		t.Fatalf("expected error message in response, got %q", resp.Msg.GetErrorMessage())
	}
}

type alwaysFailingResolver struct{ err error }

func (r *alwaysFailingResolver) Resolve(context.Context, *string) (coreemail.Provider, coreemail.ResolvedFrom, error) {
	return nil, coreemail.ResolvedFrom{}, r.err
}
