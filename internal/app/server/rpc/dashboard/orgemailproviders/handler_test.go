package orgemailproviders_test

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/authn"
	"connectrpc.com/connect"
	goredis "github.com/redis/go-redis/v9"

	"github.com/pug-sh/pug/internal/app/server/rpc"
	"github.com/pug-sh/pug/internal/app/server/rpc/dashboard/orgemailproviders"
	"github.com/pug-sh/pug/internal/apperr"
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

func strPtr(s string) *string    { return &s }
func uint32Ptr(v uint32) *uint32 { return &v }
func boolPtr(b bool) *bool       { return &b }

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
		Customer: &dbread.Customer{ID: customer.ID, Email: customer.Email},
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
		Customer: &dbread.Customer{ID: customer.ID, Email: customer.Email},
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
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != connect.CodePermissionDenied {
		t.Fatalf("expected PermissionDenied *apperr.Error, got %v", err)
	}
	if ae.Reason != apperr.ReasonOrgAdminRequired {
		t.Errorf("reason = %q, want %q", ae.Reason, apperr.ReasonOrgAdminRequired)
	}
}

// TestGetRequiresAdmin verifies a non-admin member cannot read the org's
// configured provider (which would include the redacted secret).
func TestGetRequiresAdmin(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	db := testutil.SetupPostgres(t)
	rds := testutil.SetupRedis(t)
	ctx := context.Background()

	write := dbwrite.New(db.PgW)
	read := dbread.New(db.PgRO)
	customer, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID: "cust-get-member", Email: "getmember@acme.com", DisplayName: "Member", PasswordHash: "h", PictureUri: "",
	})
	if err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}
	org, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: "org-get-member", DisplayName: "Acme"})
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
		Customer: &dbread.Customer{ID: customer.ID, Email: customer.Email},
	})
	_, err = srv.Get(ctxWithPrincipal, connect.NewRequest(&orgemailprovidersv1.GetRequest{OrgId: strPtr(org.ID)}))
	if err == nil {
		t.Fatal("expected admin error on Get")
	}
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != connect.CodePermissionDenied {
		t.Fatalf("expected PermissionDenied *apperr.Error, got %v", err)
	}
	if ae.Reason != apperr.ReasonOrgAdminRequired {
		t.Errorf("reason = %q, want %q", ae.Reason, apperr.ReasonOrgAdminRequired)
	}
}

// TestGetWithoutCipherReturnsFailedPrecondition verifies the NewServer
// contract that Get refuses to operate when the operator has not configured
// PUG_EMAIL_PROVIDER_SECRET_KEY (server constructed with cipher=nil).
func TestGetWithoutCipherReturnsFailedPrecondition(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	db := testutil.SetupPostgres(t)
	rds := testutil.SetupRedis(t)
	ctx := context.Background()

	write := dbwrite.New(db.PgW)
	read := dbread.New(db.PgRO)
	customer, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID: "cust-get-nocipher", Email: "nocipher@acme.com", DisplayName: "Admin", PasswordHash: "h", PictureUri: "",
	})
	if err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}
	org, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: "org-get-nocipher", DisplayName: "Acme"})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID: org.ID, CustomerID: customer.ID, Role: "ORG_ROLE_ADMIN",
	}); err != nil {
		t.Fatalf("CreateOrgMember: %v", err)
	}

	orgs := coreorgs.NewService(db.PgRO, db.PgW, nil)
	repo := coreemail.NewOrgProviderRepo(read, rds.Client)
	srv := orgemailproviders.NewServer(orgs, read, write, nil, repo, nil)

	ctxWithPrincipal := authn.SetInfo(ctx, &rpc.Principal{
		AuthType: rpc.AuthTypeJWT,
		Customer: &dbread.Customer{ID: customer.ID, Email: customer.Email},
	})
	_, err = srv.Get(ctxWithPrincipal, connect.NewRequest(&orgemailprovidersv1.GetRequest{OrgId: strPtr(org.ID)}))
	if err == nil {
		t.Fatal("expected FailedPrecondition when cipher unset")
	}
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != connect.CodeFailedPrecondition {
		t.Fatalf("expected FailedPrecondition *apperr.Error, got %v", err)
	}
	if ae.Reason != apperr.ReasonEmailProviderEncryptionMissing {
		t.Errorf("reason = %q, want %q", ae.Reason, apperr.ReasonEmailProviderEncryptionMissing)
	}
}

// TestGetReturnsNotFoundWhenNoProvider verifies an admin Get on an org with
// no configured provider returns CodeNotFound (not a zero-value response).
func TestGetReturnsNotFoundWhenNoProvider(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	db := testutil.SetupPostgres(t)
	rds := testutil.SetupRedis(t)
	ctx := context.Background()

	write := dbwrite.New(db.PgW)
	read := dbread.New(db.PgRO)
	customer, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID: "cust-get-empty", Email: "empty@acme.com", DisplayName: "Admin", PasswordHash: "h", PictureUri: "",
	})
	if err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}
	org, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: "org-get-empty", DisplayName: "Acme"})
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
		Customer: &dbread.Customer{ID: customer.ID, Email: customer.Email},
	})
	_, err = srv.Get(ctxWithPrincipal, connect.NewRequest(&orgemailprovidersv1.GetRequest{OrgId: strPtr(org.ID)}))
	if err == nil {
		t.Fatal("expected NotFound when no provider configured")
	}
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != connect.CodeNotFound {
		t.Fatalf("expected NotFound *apperr.Error, got %v", err)
	}
	if ae.Reason != apperr.ReasonEmailProviderNotFound {
		t.Errorf("reason = %q, want %q", ae.Reason, apperr.ReasonEmailProviderNotFound)
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
		Customer: &dbread.Customer{ID: customer.ID, Email: customer.Email},
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
		Customer: &dbread.Customer{ID: customer.ID, Email: customer.Email},
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
		Customer: &dbread.Customer{ID: adminCust.ID, Email: adminCust.Email},
	})
	memberCtx := authn.SetInfo(ctx, &rpc.Principal{
		AuthType: rpc.AuthTypeJWT,
		Customer: &dbread.Customer{ID: memberCust.ID, Email: memberCust.Email},
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

	if _, removeErr := srv.Remove(memberCtx, connect.NewRequest(&orgemailprovidersv1.RemoveRequest{OrgId: strPtr(org.ID)})); removeErr == nil {
		t.Fatal("expected admin error on Remove for member")
	} else {
		var ae *apperr.Error
		if !errors.As(removeErr, &ae) || ae.Code != connect.CodePermissionDenied {
			t.Fatalf("expected PermissionDenied *apperr.Error, got %v", removeErr)
		}
		if ae.Reason != apperr.ReasonOrgAdminRequired {
			t.Errorf("reason = %q, want %q", ae.Reason, apperr.ReasonOrgAdminRequired)
		}
	}

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

// TestSendTestRequiresAdmin verifies a non-admin member cannot trigger a
// test send (which would use the operator's reputation domain).
func TestSendTestRequiresAdmin(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	db := testutil.SetupPostgres(t)
	rds := testutil.SetupRedis(t)
	ctx := context.Background()

	write := dbwrite.New(db.PgW)
	read := dbread.New(db.PgRO)
	customer, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID: "cust-st-mem", Email: "stmember@acme.com", DisplayName: "Member", PasswordHash: "h", PictureUri: "",
	})
	if err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}
	org, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: "org-st-mem", DisplayName: "Acme"})
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
	resolver := &coreemail.OperatorOnlyResolver{Provider: &capturingProvider{}, From: "noreply@example.com"}
	mailer, err := coreemail.NewServiceWithResolver(coreemail.Config{
		DashboardBaseURL: "https://dashboard.example",
		From:             "noreply@example.com",
	}, resolver)
	if err != nil {
		t.Fatalf("NewServiceWithResolver: %v", err)
	}
	srv := orgemailproviders.NewServer(orgs, read, write, cipher, repo, mailer)

	ctxWithPrincipal := authn.SetInfo(ctx, &rpc.Principal{
		AuthType: rpc.AuthTypeJWT,
		Customer: &dbread.Customer{ID: customer.ID, Email: customer.Email},
	})
	_, err = srv.SendTest(ctxWithPrincipal, connect.NewRequest(&orgemailprovidersv1.SendTestRequest{
		OrgId:     strPtr(org.ID),
		Recipient: strPtr(customer.Email),
	}))
	if err == nil {
		t.Fatal("expected admin error")
	}
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != connect.CodePermissionDenied {
		t.Fatalf("expected PermissionDenied *apperr.Error, got %v", err)
	}
	if ae.Reason != apperr.ReasonOrgAdminRequired {
		t.Errorf("reason = %q, want %q", ae.Reason, apperr.ReasonOrgAdminRequired)
	}
}

// TestSendTestWithoutMailerReturnsFailedPrecondition verifies the NewServer
// contract that a nil mailer (operator hasn't wired SendTest in this
// deployment) yields FailedPrecondition rather than a panic.
func TestSendTestWithoutMailerReturnsFailedPrecondition(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	db := testutil.SetupPostgres(t)
	rds := testutil.SetupRedis(t)
	ctx := context.Background()

	write := dbwrite.New(db.PgW)
	read := dbread.New(db.PgRO)
	customer, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID: "cust-st-nomail", Email: "stnomail@acme.com", DisplayName: "Admin", PasswordHash: "h", PictureUri: "",
	})
	if err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}
	org, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: "org-st-nomail", DisplayName: "Acme"})
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
		Customer: &dbread.Customer{ID: customer.ID, Email: customer.Email},
	})
	_, err = srv.SendTest(ctxWithPrincipal, connect.NewRequest(&orgemailprovidersv1.SendTestRequest{
		OrgId:     strPtr(org.ID),
		Recipient: strPtr(customer.Email),
	}))
	if err == nil {
		t.Fatal("expected FailedPrecondition when mailer unset")
	}
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != connect.CodeFailedPrecondition {
		t.Fatalf("expected FailedPrecondition *apperr.Error, got %v", err)
	}
	if ae.Reason != apperr.ReasonEmailTestSendUnavailable {
		t.Errorf("reason = %q, want %q", ae.Reason, apperr.ReasonEmailTestSendUnavailable)
	}
}

// TestSendTestRejectsForeignRecipient pins the recipient-restriction rule:
// admins may only test-send to their own email, preventing the operator's
// reputation domain from becoming a free phishing seed channel.
func TestSendTestRejectsForeignRecipient(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	db := testutil.SetupPostgres(t)
	rds := testutil.SetupRedis(t)
	ctx := context.Background()

	write := dbwrite.New(db.PgW)
	read := dbread.New(db.PgRO)
	customer, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID: "cust-st-foreign", Email: "stforeign@acme.com", DisplayName: "Admin", PasswordHash: "h", PictureUri: "",
	})
	if err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}
	org, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: "org-st-foreign", DisplayName: "Acme"})
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
	resolver := &coreemail.OperatorOnlyResolver{Provider: &capturingProvider{}, From: "noreply@example.com"}
	mailer, err := coreemail.NewServiceWithResolver(coreemail.Config{
		DashboardBaseURL: "https://dashboard.example",
		From:             "noreply@example.com",
	}, resolver)
	if err != nil {
		t.Fatalf("NewServiceWithResolver: %v", err)
	}
	srv := orgemailproviders.NewServer(orgs, read, write, cipher, repo, mailer)

	ctxWithPrincipal := authn.SetInfo(ctx, &rpc.Principal{
		AuthType: rpc.AuthTypeJWT,
		Customer: &dbread.Customer{ID: customer.ID, Email: customer.Email},
	})
	_, err = srv.SendTest(ctxWithPrincipal, connect.NewRequest(&orgemailprovidersv1.SendTestRequest{
		OrgId:     strPtr(org.ID),
		Recipient: strPtr("someone-else@evil.example"),
	}))
	if err == nil {
		t.Fatal("expected PermissionDenied for non-admin recipient")
	}
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != connect.CodePermissionDenied {
		t.Fatalf("expected PermissionDenied *apperr.Error, got %v", err)
	}
	if ae.Reason != apperr.ReasonEmailTestRecipientMismatch {
		t.Errorf("reason = %q, want %q", ae.Reason, apperr.ReasonEmailTestRecipientMismatch)
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
	ctxWithPrincipal := authn.SetInfo(ctx, &rpc.Principal{
		AuthType: rpc.AuthTypeJWT,
		Customer: &dbread.Customer{ID: customer.ID, Email: customer.Email},
	})

	resp, err := srv.SendTest(ctxWithPrincipal, connect.NewRequest(&orgemailprovidersv1.SendTestRequest{
		OrgId: strPtr(org.ID), Recipient: strPtr(customer.Email),
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

type capturingProvider struct {
	got   coreemail.Message
	all   []coreemail.Message
	calls int
}

func (p *capturingProvider) Send(_ context.Context, msg coreemail.Message) error {
	p.got = msg
	p.all = append(p.all, msg)
	p.calls++
	return nil
}

// TestSendTestHappyPath asserts that on a successful provider Send the handler
// returns Success=true and the recorded provider received the rendered test
// message (correct recipient and subject containing "test").
func TestSendTestHappyPath(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	db := testutil.SetupPostgres(t)
	rds := testutil.SetupRedis(t)
	ctx := context.Background()

	write := dbwrite.New(db.PgW)
	read := dbread.New(db.PgRO)

	customer, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID: "cust-happy-1", Email: "happy-admin@acme.com", DisplayName: "Admin", PasswordHash: "h", PictureUri: "",
	})
	if err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}
	org, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: "org-happy-1", DisplayName: "Acme"})
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

	fake := &capturingProvider{}
	resolver := &coreemail.OperatorOnlyResolver{Provider: fake, From: "noreply@example.com"}
	mailer, err := coreemail.NewServiceWithResolver(coreemail.Config{
		DashboardBaseURL: "https://dashboard.example",
		From:             "noreply@example.com",
	}, resolver)
	if err != nil {
		t.Fatalf("NewServiceWithResolver: %v", err)
	}

	srv := orgemailproviders.NewServer(orgs, read, write, cipher, repo, mailer)
	ctxWithPrincipal := authn.SetInfo(ctx, &rpc.Principal{
		AuthType: rpc.AuthTypeJWT,
		Customer: &dbread.Customer{ID: customer.ID, Email: customer.Email},
	})

	resp, err := srv.SendTest(ctxWithPrincipal, connect.NewRequest(&orgemailprovidersv1.SendTestRequest{
		OrgId: strPtr(org.ID), Recipient: strPtr(customer.Email),
	}))
	if err != nil {
		t.Fatalf("SendTest: %v", err)
	}
	if !resp.Msg.GetSuccess() {
		t.Fatalf("expected Success=true, got error %q", resp.Msg.GetErrorMessage())
	}
	if fake.calls != 1 {
		t.Fatalf("expected 1 Send call, got %d", fake.calls)
	}
	if fake.got.To != customer.Email {
		t.Fatalf("To: got %q want %q", fake.got.To, customer.Email)
	}
	if !strings.Contains(fake.got.Subject, "test") {
		t.Fatalf("expected 'test' in subject, got %q", fake.got.Subject)
	}
}

func TestSendTestUsesFreshIdempotencyKeyPerAttempt(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	db := testutil.SetupPostgres(t)
	rds := testutil.SetupRedis(t)
	ctx := context.Background()

	write := dbwrite.New(db.PgW)
	read := dbread.New(db.PgRO)

	customer, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID: "cust-happy-2", Email: "again-admin@acme.com", DisplayName: "Admin", PasswordHash: "h", PictureUri: "",
	})
	if err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}
	org, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: "org-happy-2", DisplayName: "Acme"})
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

	fake := &capturingProvider{}
	resolver := &coreemail.OperatorOnlyResolver{Provider: fake, From: "noreply@example.com"}
	mailer, err := coreemail.NewServiceWithResolver(coreemail.Config{
		DashboardBaseURL: "https://dashboard.example",
		From:             "noreply@example.com",
	}, resolver)
	if err != nil {
		t.Fatalf("NewServiceWithResolver: %v", err)
	}

	srv := orgemailproviders.NewServer(orgs, read, write, cipher, repo, mailer)
	ctxWithPrincipal := authn.SetInfo(ctx, &rpc.Principal{
		AuthType: rpc.AuthTypeJWT,
		Customer: &dbread.Customer{ID: customer.ID, Email: customer.Email},
	})
	req := connect.NewRequest(&orgemailprovidersv1.SendTestRequest{
		OrgId: strPtr(org.ID), Recipient: strPtr(customer.Email),
	})

	if _, err := srv.SendTest(ctxWithPrincipal, req); err != nil {
		t.Fatalf("first SendTest: %v", err)
	}
	if _, err := srv.SendTest(ctxWithPrincipal, req); err != nil {
		t.Fatalf("second SendTest: %v", err)
	}
	if len(fake.all) != 2 {
		t.Fatalf("expected 2 captured messages, got %d (calls=%d)", len(fake.all), fake.calls)
	}
	if fake.all[0].IdempotencyKey == fake.all[1].IdempotencyKey {
		t.Fatalf("expected distinct idempotency keys, got %q", fake.all[0].IdempotencyKey)
	}
	prefix := "send_test:" + org.ID + ":" + customer.Email + ":"
	for i, msg := range fake.all {
		if !strings.HasPrefix(msg.IdempotencyKey, prefix) {
			t.Fatalf("idempotency key[%d] missing tenant prefix: got %q, want prefix %q", i, msg.IdempotencyKey, prefix)
		}
	}
}

// TestSetReturnsInternalOnInvalidateFailure pins the load-bearing-Invalidate
// contract. With the bracketed Invalidate→Upsert→Invalidate ordering, a
// Redis failure on the FIRST invalidate aborts before the Upsert runs —
// the DB stays unchanged and the admin gets CodeInternal so they retry.
// Without bracketing, a Redis hiccup after a successful Upsert would leave
// stale ciphertext serving real email sends for the cache TTL window.
//
// We point the repo's Redis client at a closed port so DEL fails reliably.
func TestSetReturnsInternalOnInvalidateFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	db := testutil.SetupPostgres(t)
	ctx := context.Background()

	write := dbwrite.New(db.PgW)
	read := dbread.New(db.PgRO)
	customer, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID: "cust-inv-fail", Email: "admin-inv@acme.com", DisplayName: "Admin", PasswordHash: "h", PictureUri: "",
	})
	if err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}
	org, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: "org-inv-fail", DisplayName: "Acme"})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID: org.ID, CustomerID: customer.ID, Role: "ORG_ROLE_ADMIN",
	}); err != nil {
		t.Fatalf("CreateOrgMember: %v", err)
	}

	broken := goredis.NewClient(&goredis.Options{
		Addr:         "127.0.0.1:1",
		DialTimeout:  200 * time.Millisecond,
		ReadTimeout:  200 * time.Millisecond,
		WriteTimeout: 200 * time.Millisecond,
		MaxRetries:   -1,
	})
	t.Cleanup(func() { _ = broken.Close() })

	orgs := coreorgs.NewService(db.PgRO, db.PgW, nil)
	cipher := setupCipher(t)
	repo := coreemail.NewOrgProviderRepo(read, broken)
	srv := orgemailproviders.NewServer(orgs, read, write, cipher, repo, nil)

	ctxWithPrincipal := authn.SetInfo(ctx, &rpc.Principal{
		AuthType: rpc.AuthTypeJWT,
		Customer: &dbread.Customer{ID: customer.ID, Email: customer.Email},
	})
	_, err = srv.Set(ctxWithPrincipal, connect.NewRequest(&orgemailprovidersv1.SetRequest{
		OrgId:       strPtr(org.ID),
		FromAddress: strPtr("ops@acme.com"),
		Config: &orgemailprovidersv1.SetRequest_Resend{
			Resend: &orgemailprovidersv1.ResendConfig{ApiKey: strPtr("sk_test_abcdef1234567890")},
		},
	}))
	if err == nil {
		t.Fatal("expected Set to fail when Invalidate cannot reach cache")
	}
	if got := connect.CodeOf(err); got != connect.CodeInternal {
		t.Fatalf("expected CodeInternal, got %v (err=%v)", got, err)
	}

	// Sanity: with bracketed invalidate ordering, the first Invalidate fails
	// before the Upsert runs, so no row should exist. This pins the
	// atomic-ish "no DB write without a successful invalidate" contract —
	// a regression that moved Upsert before Invalidate would leave a row
	// behind here.
	if _, err := read.GetOrgEmailProvider(ctx, org.ID); err == nil {
		t.Fatal("expected no row after Invalidate-failure aborted before Upsert; got a row")
	}
}
