package auth_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/protobuf/proto"

	"github.com/pug-sh/pug/internal/core/auth"
	natsdeps "github.com/pug-sh/pug/internal/deps/nats"
	orgsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/orgs/v1"
	emailworkerv1 "github.com/pug-sh/pug/internal/gen/proto/workers/email/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
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

	t.Run("SignUpWithEmail", func(t *testing.T) {
		token, err := svc.SignUpWithEmail(ctx, "test@example.com", "password123")
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
		payload := publisher.jobs[0].job.GetMagicLink()
		if payload == nil {
			t.Fatal("expected magic-link payload from sign-up")
		}
		magicLinkToken := payload.GetToken()
		if magicLinkToken == "" {
			t.Fatal("expected non-empty magic-link token")
		}

		if _, err := read.GetValidEmailActionTokenByHashAndPurpose(ctx, dbread.GetValidEmailActionTokenByHashAndPurposeParams{
			TokenHash: hashToken(magicLinkToken),
			Purpose:   "magic_link",
		}); err != nil {
			t.Fatalf("GetValidEmailActionTokenByHashAndPurpose(signup magic link): %v", err)
		}

		// Verify the CreateOrgWithDefaultsInTx wiring: the new customer is the
		// admin of exactly one org with exactly one default project. A
		// regression that broke the wiring (wrong customer ID, helper called
		// outside the tx) would not be caught by the customer/token
		// assertions above.
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
		if _, err := svc.SignUpWithEmail(ctx, "test@example.com", "password123"); err == nil {
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

// The signup publish-failure alarm contract for emails.publish_failure_total
// in the auth package (exercised by the signup_magic_link subtest below). If
// publishing the signup magic-link job fails after tx commit:
//
//   - SignUpWithEmail must NOT return an error (fire-and-forget; the user has
//     a valid account and JWT).
//   - The customer + org + admin-member + default project rows must all
//     remain in PG (tx already committed before publish).
//   - The counter must tick with kind="magic_link".
//
// Without the counter, a regression here would silently drop signup
// magic-link emails for every new user.
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

// TestEmailPublishFailureCountersByKind pins the alarm contract: every
// payload-kind path through publishEmailJob must tick
// emails.publish_failure_total with the right {kind} attribute when the
// underlying publish fails.
//
// All sub-tests share one MeterProvider because otel-go's global delegating
// instruments lock to the FIRST SetMeterProvider call; SetMeterProvider in
// each sub-test would silently route ticks to the first sub-test's reader.
// One provider, one reader, sub-tests filter by kind.
func TestEmailPublishFailureCountersByKind(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	reader := sdkmetric.NewManualReader()
	prevProvider := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(prevProvider) })

	db := testutil.SetupPostgres(t)
	svc := auth.NewService(db.PgRO, db.PgW, []byte("test-secret-key-for-jwt"), failingPublisher{})
	ctx := context.Background()

	t.Run("signup_magic_link", func(t *testing.T) {
		const email = "signup-publish-failure@example.com"
		token, err := svc.SignUpWithEmail(ctx, email, "password123")
		if err != nil {
			t.Fatalf("SignUpWithEmail should swallow publish failure, got: %v", err)
		}
		if token == "" {
			t.Fatal("expected JWT despite publish failure")
		}

		read := dbread.New(db.PgW)
		customer, err := read.GetCustomerByEmail(ctx, email)
		if err != nil {
			t.Fatalf("GetCustomerByEmail: %v", err)
		}
		// SignUp must commit the org + admin membership + default project
		// regardless of publish failure (fire-and-forget contract).
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

		assertAuthEmailFailureCounter(t, reader, "magic_link")
	})

	t.Run("magic_link", func(t *testing.T) {
		// RequestMagicLink always proceeds to publish regardless of whether an
		// account exists (no account-existence oracle), so no customer setup is
		// needed. The publish fails, the error is swallowed, and the counter
		// must tick with kind="magic_link".
		if err := svc.RequestMagicLink(ctx, "magic-link-publish-failure@example.com"); err != nil {
			t.Fatalf("RequestMagicLink should swallow publish failure, got: %v", err)
		}
		assertAuthEmailFailureCounter(t, reader, "magic_link")
	})

	t.Run("unknown", func(t *testing.T) {
		// Direct PublishEmailJobForTest call with an EmailJob whose Payload
		// oneof is unset hits the default branch of payloadKindFromJob — the
		// only counter bucket that fires on proto drift, so it must be
		// observable to operators.
		svc.PublishEmailJobForTest(ctx, &emailworkerv1.EmailJob{})
		assertAuthEmailFailureCounter(t, reader, "unknown")
	})
}
