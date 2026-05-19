package auth_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/rs/xid"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/protobuf/proto"

	"github.com/pug-sh/pug/internal/core/auth"
	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	natsdeps "github.com/pug-sh/pug/internal/deps/nats"
	orgsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/orgs/v1"
	emailworkerv1 "github.com/pug-sh/pug/internal/gen/proto/workers/email/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
)

type publishedJob struct {
	subject string
	job     *emailworkerv1.EmailJob
}

type stubPublisher struct {
	jobs []publishedJob
}

func (p *stubPublisher) Publish(_ context.Context, subject string, data []byte) error {
	job := &emailworkerv1.EmailJob{}
	if err := proto.Unmarshal(data, job); err != nil {
		return err
	}
	p.jobs = append(p.jobs, publishedJob{subject: subject, job: job})
	return nil
}

func TestAuthService(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	db := testutil.SetupPostgres(t)
	jwtKey := []byte("test-secret-key-for-jwt")
	publisher := &stubPublisher{}
	svc := auth.NewService(db.PgRO, db.PgW, jwtKey, publisher)
	read := dbread.New(db.PgW)
	ctx := context.Background()

	var signupToken string
	var verifyToken1 string
	var verifyToken2 string
	var resetToken string

	t.Run("SignUpWithEmail", func(t *testing.T) {
		token, err := svc.SignUpWithEmail(ctx, "test@example.com", "password123", "")
		if err != nil {
			t.Fatalf("SignUpWithEmail: %v", err)
		}
		if token == "" {
			t.Fatal("expected non-empty token")
		}
		signupToken = token

		customer, err := read.GetCustomerByEmail(ctx, "test@example.com")
		if err != nil {
			t.Fatalf("GetCustomerByEmail: %v", err)
		}
		if customer.EmailVerifiedAt.Valid {
			t.Fatal("expected newly created customer to be unverified")
		}

		if len(publisher.jobs) != 1 {
			t.Fatalf("expected 1 published job, got %d", len(publisher.jobs))
		}
		if publisher.jobs[0].subject != natsdeps.MiscEmailJobsSubject {
			t.Fatalf("subject = %q, want %q", publisher.jobs[0].subject, natsdeps.MiscEmailJobsSubject)
		}
		payload := publisher.jobs[0].job.GetSignupVerifyWelcome()
		if payload == nil {
			t.Fatal("expected signup verify welcome payload")
		}
		verifyToken1 = payload.GetToken()
		if verifyToken1 == "" {
			t.Fatal("expected non-empty verification token")
		}

		if _, err := read.GetValidEmailActionTokenByHashAndPurpose(ctx, dbread.GetValidEmailActionTokenByHashAndPurposeParams{
			TokenHash: hashToken(verifyToken1),
			Purpose:   "verify_email",
		}); err != nil {
			t.Fatalf("GetValidEmailActionTokenByHashAndPurpose(signup verify): %v", err)
		}

		// Verify the CreateOrgWithDefaultsInTx wiring extracted in 666cdb2: the
		// new customer is the admin of exactly one org with exactly one default
		// project. A regression that broke the wiring (wrong customer ID,
		// helper called outside the tx) would not be caught by the existing
		// customer/token assertions above.
		var adminMemberships, projectCount int
		if err := db.PgW.QueryRow(ctx,
			`select count(*) from org_members where customer_id = $1 and role = $2`,
			customer.ID, orgsv1.OrgRole_ORG_ROLE_ADMIN.String(),
		).Scan(&adminMemberships); err != nil {
			t.Fatalf("scan admin memberships: %v", err)
		}
		if adminMemberships != 1 {
			t.Fatalf("want 1 admin membership for new signup, got %d", adminMemberships)
		}
		if err := db.PgW.QueryRow(ctx,
			`select count(*) from projects p join org_members m on m.org_id = p.org_id where m.customer_id = $1`,
			customer.ID,
		).Scan(&projectCount); err != nil {
			t.Fatalf("scan default projects: %v", err)
		}
		if projectCount != 1 {
			t.Fatalf("want 1 default project for new signup, got %d", projectCount)
		}
	})

	t.Run("SignUpWithEmail_duplicate", func(t *testing.T) {
		if _, err := svc.SignUpWithEmail(ctx, "test@example.com", "password123", ""); err == nil {
			t.Fatal("expected error for duplicate email")
		} else if !errors.Is(err, auth.ErrEmailAlreadyExists) {
			t.Fatalf("expected ErrEmailAlreadyExists, got %v", err)
		}
	})

	t.Run("SignInWithEmail_valid", func(t *testing.T) {
		before := len(publisher.jobs)
		token, err := svc.SignInWithEmail(ctx, "test@example.com", "password123")
		if err != nil {
			t.Fatalf("SignInWithEmail: %v", err)
		}
		if token == "" {
			t.Fatal("expected non-empty token")
		}
		if len(publisher.jobs) != before {
			t.Fatal("sign in should not publish email jobs")
		}
	})

	t.Run("ResendVerificationEmail_invalidates_previous_token", func(t *testing.T) {
		if err := svc.ResendVerificationEmail(ctx, "test@example.com"); err != nil {
			t.Fatalf("ResendVerificationEmail: %v", err)
		}
		if len(publisher.jobs) != 2 {
			t.Fatalf("expected 2 published jobs after resend, got %d", len(publisher.jobs))
		}
		payload := publisher.jobs[1].job.GetVerificationResend()
		if payload == nil {
			t.Fatal("expected verification resend payload")
		}
		verifyToken2 = payload.GetToken()
		if verifyToken2 == "" || verifyToken2 == verifyToken1 {
			t.Fatal("expected fresh verification resend token")
		}

		if _, err := read.GetValidEmailActionTokenByHashAndPurpose(ctx, dbread.GetValidEmailActionTokenByHashAndPurposeParams{
			TokenHash: hashToken(verifyToken1),
			Purpose:   "verify_email",
		}); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("expected prior verification token to be invalidated, got %v", err)
		}
		if _, err := read.GetValidEmailActionTokenByHashAndPurpose(ctx, dbread.GetValidEmailActionTokenByHashAndPurposeParams{
			TokenHash: hashToken(verifyToken2),
			Purpose:   "verify_email",
		}); err != nil {
			t.Fatalf("expected fresh verification token to be active: %v", err)
		}
	})

	t.Run("VerifyEmail_rejects_invalidated_token", func(t *testing.T) {
		if err := svc.VerifyEmail(ctx, verifyToken1); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("expected ErrInvalidToken, got %v", err)
		}
	})

	t.Run("VerifyEmail_consumes_valid_token", func(t *testing.T) {
		if err := svc.VerifyEmail(ctx, verifyToken2); err != nil {
			t.Fatalf("VerifyEmail: %v", err)
		}
		customer, err := read.GetCustomerByEmail(ctx, "test@example.com")
		if err != nil {
			t.Fatalf("GetCustomerByEmail: %v", err)
		}
		if !customer.EmailVerifiedAt.Valid {
			t.Fatal("expected customer email to be marked verified")
		}
		if err := svc.VerifyEmail(ctx, verifyToken2); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("expected reused token to fail, got %v", err)
		}
	})

	t.Run("RequestPasswordReset_non_enumerating", func(t *testing.T) {
		before := len(publisher.jobs)
		if err := svc.RequestPasswordReset(ctx, "missing@example.com"); err != nil {
			t.Fatalf("RequestPasswordReset(missing): %v", err)
		}
		if len(publisher.jobs) != before {
			t.Fatal("missing customer should not publish a reset job")
		}

		if err := svc.RequestPasswordReset(ctx, "test@example.com"); err != nil {
			t.Fatalf("RequestPasswordReset(existing): %v", err)
		}
		if len(publisher.jobs) != before+1 {
			t.Fatalf("expected reset job to be published, got %d jobs", len(publisher.jobs))
		}
		payload := publisher.jobs[len(publisher.jobs)-1].job.GetPasswordReset()
		if payload == nil {
			t.Fatal("expected password reset payload")
		}
		resetToken = payload.GetToken()
	})

	t.Run("ResetPassword_updates_hash_and_consumes_token", func(t *testing.T) {
		if err := svc.ResetPassword(ctx, resetToken, "new-password123"); err != nil {
			t.Fatalf("ResetPassword: %v", err)
		}
		if _, err := svc.SignInWithEmail(ctx, "test@example.com", "password123"); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("expected old password to fail, got %v", err)
		}
		if _, err := svc.SignInWithEmail(ctx, "test@example.com", "new-password123"); err != nil {
			t.Fatalf("expected new password to work, got %v", err)
		}
		if err := svc.ResetPassword(ctx, resetToken, "another-password"); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("expected reused reset token to fail, got %v", err)
		}
	})

	t.Run("JWT_structure", func(t *testing.T) {
		parsed, err := jwt.Parse(signupToken, func(t *jwt.Token) (any, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, errors.New("unexpected signing method")
			}
			return jwtKey, nil
		})
		if err != nil {
			t.Fatalf("JWT parse: %v", err)
		}
		if !parsed.Valid {
			t.Fatal("JWT is not valid")
		}
	})
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// failingPublisher returns an error from every Publish so we can exercise the
// fire-and-forget silent-drop path in SignUpWithEmail without involving NATS.
type failingPublisher struct{}

func (failingPublisher) Publish(_ context.Context, _ string, _ []byte) error {
	return errors.New("simulated publish failure")
}

// TestSignUpWithEmailCountsPublishFailure pins the alarm contract for
// emails.publish_failure_total in the auth package. If publishing the welcome
// verify-email job fails after tx commit:
//
//   - SignUpWithEmail must NOT return an error (fire-and-forget; the user has
//     a valid account and JWT).
//   - The customer + org + admin-member + default project rows must all
//     remain in PG (tx already committed before publish).
//   - The counter must tick with kind="signup_verify_welcome".
//
// Without the counter, a regression here would silently drop signup
// confirmation emails for every new user.
func TestSignUpWithEmailCountsPublishFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	reader := sdkmetric.NewManualReader()
	prevProvider := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(prevProvider) })

	db := testutil.SetupPostgres(t)
	svc := auth.NewService(db.PgRO, db.PgW, []byte("test-secret-key-for-jwt"), failingPublisher{})
	read := dbread.New(db.PgW)
	ctx := context.Background()

	const email = "publish-failure@example.com"
	token, err := svc.SignUpWithEmail(ctx, email, "password123", "")
	if err != nil {
		t.Fatalf("SignUpWithEmail should swallow publish failure, got: %v", err)
	}
	if token == "" {
		t.Fatal("expected JWT despite publish failure")
	}

	customer, err := read.GetCustomerByEmail(ctx, email)
	if err != nil {
		t.Fatalf("GetCustomerByEmail: %v", err)
	}
	// The CreateOrgWithDefaultsInTx wiring inside SignUpWithEmail must have
	// committed: one org, one admin membership, one default project.
	var orgCount, projCount int
	if err := db.PgW.QueryRow(ctx,
		`select count(*) from org_members where customer_id = $1 and role = $2`,
		customer.ID, orgsv1.OrgRole_ORG_ROLE_ADMIN.String(),
	).Scan(&orgCount); err != nil {
		t.Fatalf("scan admin memberships: %v", err)
	}
	if orgCount != 1 {
		t.Fatalf("want 1 admin membership for new signup, got %d", orgCount)
	}
	if err := db.PgW.QueryRow(ctx,
		`select count(*) from projects p join org_members m on m.org_id = p.org_id where m.customer_id = $1`,
		customer.ID,
	).Scan(&projCount); err != nil {
		t.Fatalf("scan projects: %v", err)
	}
	if projCount != 1 {
		t.Fatalf("want 1 default project for new signup, got %d", projCount)
	}

	assertAuthEmailFailureCounter(t, reader, "signup_verify_welcome")
}

// assertAuthEmailFailureCounter mirrors the orgs-test helper. Lives here
// (not as a shared helper) to keep the test packages independent.
func assertAuthEmailFailureCounter(t *testing.T, reader sdkmetric.Reader, kind string) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("reader.Collect: %v", err)
	}
	const scope = "github.com/pug-sh/pug/internal/core/auth"
	var total int64
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != scope {
			continue
		}
		for _, m := range sm.Metrics {
			if m.Name != "emails.publish_failure_total" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("emails.publish_failure_total: want Sum[int64], got %T", m.Data)
			}
			for _, dp := range sum.DataPoints {
				if got, ok := dp.Attributes.Value("kind"); ok && got.AsString() == kind {
					total += dp.Value
				}
			}
		}
	}
	if total == 0 {
		t.Fatalf("expected emails.publish_failure_total{kind=%q} > 0", kind)
	}
}

// TestVerifyEmailRejectsResetPasswordToken pins purpose-isolation: a token
// issued for password reset must NOT be redeemable as a verify-email token.
// If the purpose constants were ever collapsed (or the gate accidentally
// dropped from the query), an attacker who phished a reset link could redeem
// it as a verify-email token (or vice versa). Both directions are tested.
func TestEmailActionTokenPurposeIsolation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	db := testutil.SetupPostgres(t)
	publisher := &stubPublisher{}
	svc := auth.NewService(db.PgRO, db.PgW, []byte("test-secret-key-for-jwt"), publisher)
	ctx := context.Background()

	t.Run("verify_email_rejects_reset_password_token", func(t *testing.T) {
		const email = "purpose-1@example.com"
		if _, err := svc.SignUpWithEmail(ctx, email, "password123", ""); err != nil {
			t.Fatalf("SignUpWithEmail: %v", err)
		}
		// Drain the welcome verify token.
		verifyTok := publisher.jobs[len(publisher.jobs)-1].job.GetSignupVerifyWelcome().GetToken()
		if err := svc.VerifyEmail(ctx, verifyTok); err != nil {
			t.Fatalf("VerifyEmail seed: %v", err)
		}
		// Issue a reset_password token.
		if err := svc.RequestPasswordReset(ctx, email); err != nil {
			t.Fatalf("RequestPasswordReset: %v", err)
		}
		resetTok := publisher.jobs[len(publisher.jobs)-1].job.GetPasswordReset().GetToken()
		// Attempt to redeem the reset token as a verify token.
		if err := svc.VerifyEmail(ctx, resetTok); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("expected ErrInvalidToken when verifying with a reset token, got %v", err)
		}
	})

	t.Run("reset_password_rejects_verify_email_token", func(t *testing.T) {
		const email = "purpose-2@example.com"
		if _, err := svc.SignUpWithEmail(ctx, email, "password123", ""); err != nil {
			t.Fatalf("SignUpWithEmail: %v", err)
		}
		verifyTok := publisher.jobs[len(publisher.jobs)-1].job.GetSignupVerifyWelcome().GetToken()
		// Attempt to redeem the verify token as a reset token.
		if err := svc.ResetPassword(ctx, verifyTok, "new-password123"); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("expected ErrInvalidToken when resetting with a verify token, got %v", err)
		}
	})
}

// TestEmailActionTokenExpiredRejection pins the `expires_at > now()` predicate
// at the service-call layer (rather than only transitively via "already
// consumed"). If the SQL predicate were ever removed during a query rewrite,
// an attacker who learned a stale token would gain indefinite reuse.
func TestEmailActionTokenExpiredRejection(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	db := testutil.SetupPostgres(t)
	publisher := &stubPublisher{}
	svc := auth.NewService(db.PgRO, db.PgW, []byte("test-secret-key-for-jwt"), publisher)
	write := dbwrite.New(db.PgW)
	ctx := context.Background()

	const email = "expired@example.com"
	if _, err := svc.SignUpWithEmail(ctx, email, "password123", ""); err != nil {
		t.Fatalf("SignUpWithEmail: %v", err)
	}
	customer, err := dbread.New(db.PgW).GetCustomerByEmail(ctx, email)
	if err != nil {
		t.Fatalf("GetCustomerByEmail: %v", err)
	}

	insertExpired := func(t *testing.T, purpose string) string {
		t.Helper()
		raw := xid.New().String() + xid.New().String()
		if _, err := write.CreateEmailActionToken(ctx, dbwrite.CreateEmailActionTokenParams{
			ID:         xid.New().String(),
			CustomerID: pgtype.Text{String: customer.ID, Valid: true},
			Email:      email,
			Purpose:    purpose,
			TokenHash:  hashToken(raw),
			ExpiresAt:  pgtype.Timestamptz{Time: time.Now().Add(-1 * time.Hour), Valid: true},
		}); err != nil {
			t.Fatalf("CreateEmailActionToken: %v", err)
		}
		return raw
	}

	t.Run("verify_email_rejects_expired_token", func(t *testing.T) {
		tok := insertExpired(t, "verify_email")
		if err := svc.VerifyEmail(ctx, tok); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("expected ErrInvalidToken for expired verify token, got %v", err)
		}
	})

	t.Run("reset_password_rejects_expired_token", func(t *testing.T) {
		tok := insertExpired(t, "reset_password")
		if err := svc.ResetPassword(ctx, tok, "new-pass123"); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("expected ErrInvalidToken for expired reset token, got %v", err)
		}
	})
}

// TestSignUpWithEmail_InviteHappyPath pins the invite-driven signup contract:
// a valid invite_token at signup adds the new customer to the invited org as
// MEMBER, auto-verifies their email (inbox access proved by invite delivery),
// and skips both the default-org creation and the verification email job.
// A regression that re-introduced the default org would show up as either
// extra org_members rows or a SignupVerifyWelcome publish.
func TestSignUpWithEmail_InviteHappyPath(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	db := testutil.SetupPostgres(t)
	ctx := context.Background()

	// Separate publisher for the orgs service so the invite-email publish
	// doesn't pollute the auth publisher's job list.
	orgsPub := &stubPublisher{}
	orgSvc := coreorgs.NewService(db.PgRO, db.PgW, orgsPub)

	inviterEmail := "inviter-" + xid.New().String() + "@example.com"
	inviterID := xid.New().String()
	write := dbwrite.New(db.PgW)
	if _, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID: inviterID, Email: inviterEmail, DisplayName: "Inviter", PasswordHash: "hash",
	}); err != nil {
		t.Fatalf("CreateCustomer inviter: %v", err)
	}
	org, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{
		ID: xid.New().String(), DisplayName: "Acme",
	})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID: org.ID, CustomerID: inviterID, Role: orgsv1.OrgRole_ORG_ROLE_ADMIN.String(),
	}); err != nil {
		t.Fatalf("CreateOrgMember: %v", err)
	}

	inviteeEmail := "invitee-" + xid.New().String() + "@example.com"
	inv, err := orgSvc.InviteMember(ctx, org.ID, inviterID, inviteeEmail)
	if err != nil {
		t.Fatalf("InviteMember: %v", err)
	}

	authPub := &stubPublisher{}
	authSvc := auth.NewService(db.PgRO, db.PgW, []byte("test-secret-key-for-jwt"), authPub)

	if _, err := authSvc.SignUpWithEmail(ctx, inviteeEmail, "password123", inv.Token); err != nil {
		t.Fatalf("SignUpWithEmail with invite token: %v", err)
	}

	read := dbread.New(db.PgW)
	customer, err := read.GetCustomerByEmail(ctx, inviteeEmail)
	if err != nil {
		t.Fatalf("GetCustomerByEmail: %v", err)
	}
	if !customer.EmailVerifiedAt.Valid {
		t.Fatal("expected invite-driven signup to auto-verify email")
	}

	// Exactly one membership row, in the invited org, role MEMBER.
	var memberRole string
	var memberOrgID string
	if err := db.PgW.QueryRow(ctx,
		`select org_id, role from org_members where customer_id = $1`,
		customer.ID,
	).Scan(&memberOrgID, &memberRole); err != nil {
		t.Fatalf("scan membership: %v", err)
	}
	if memberOrgID != org.ID {
		t.Fatalf("membership org_id = %q, want %q", memberOrgID, org.ID)
	}
	if memberRole != orgsv1.OrgRole_ORG_ROLE_MEMBER.String() {
		t.Fatalf("membership role = %q, want MEMBER", memberRole)
	}

	// No second org created for the invitee — the invited org has one project
	// from before-signup setup (zero in our case), but the invitee should have
	// zero "default" projects of their own.
	var defaultProjects int
	if err := db.PgW.QueryRow(ctx,
		`select count(*) from projects p
		 join org_members m on m.org_id = p.org_id
		 where m.customer_id = $1 and p.display_name = 'default'`,
		customer.ID,
	).Scan(&defaultProjects); err != nil {
		t.Fatalf("scan default projects: %v", err)
	}
	if defaultProjects != 0 {
		t.Fatalf("expected 0 default projects for invite-driven signup, got %d", defaultProjects)
	}

	// No verification email job published — invite delivery already proved
	// inbox access.
	for _, j := range authPub.jobs {
		if j.job.GetSignupVerifyWelcome() != nil {
			t.Fatal("invite-driven signup must not publish SignupVerifyWelcome")
		}
	}
}

// TestSignUpWithEmail_InviteFallback pins the bad-token fallback path: an
// invalid invite_token (token never existed) is treated as if no token was
// passed — customer is created, default org+project seeded, verification
// email queued. The customer is NOT auto-verified because invite-delivery
// proof did not actually happen.
func TestSignUpWithEmail_InviteFallback(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	db := testutil.SetupPostgres(t)
	ctx := context.Background()
	pub := &stubPublisher{}
	svc := auth.NewService(db.PgRO, db.PgW, []byte("test-secret-key-for-jwt"), pub)

	email := "fallback-" + xid.New().String() + "@example.com"
	if _, err := svc.SignUpWithEmail(ctx, email, "password123", "this-token-does-not-exist"); err != nil {
		t.Fatalf("SignUpWithEmail with bad invite token: %v", err)
	}

	read := dbread.New(db.PgW)
	customer, err := read.GetCustomerByEmail(ctx, email)
	if err != nil {
		t.Fatalf("GetCustomerByEmail: %v", err)
	}
	if customer.EmailVerifiedAt.Valid {
		t.Fatal("fallback signup must not auto-verify email — invite was bogus")
	}

	var defaultProjects int
	if err := db.PgW.QueryRow(ctx,
		`select count(*) from projects p
		 join org_members m on m.org_id = p.org_id
		 where m.customer_id = $1 and p.display_name = 'default'`,
		customer.ID,
	).Scan(&defaultProjects); err != nil {
		t.Fatalf("scan default projects: %v", err)
	}
	if defaultProjects != 1 {
		t.Fatalf("expected 1 default project on fallback, got %d", defaultProjects)
	}

	var verifyJobs int
	for _, j := range pub.jobs {
		if j.job.GetSignupVerifyWelcome() != nil {
			verifyJobs++
		}
	}
	if verifyJobs != 1 {
		t.Fatalf("expected 1 SignupVerifyWelcome publish on fallback, got %d", verifyJobs)
	}
}
