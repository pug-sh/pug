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
	"golang.org/x/crypto/bcrypt"
	"google.golang.org/protobuf/proto"

	"github.com/pug-sh/pug/internal/core/auth"
	emailworkerv1 "github.com/pug-sh/pug/internal/gen/proto/workers/email/v1"
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
	svc := auth.NewService(db.PgRO, db.PgW, jwtKey, &stubPublisher{})
	write := dbwrite.New(db.PgW)
	ctx := context.Background()

	// Password signup was removed; the supported way to gain a password is the
	// authenticated SetPassword flow. Seed the bcrypt hash directly here to
	// exercise SignInWithEmail without that round-trip.
	hash, err := bcrypt.GenerateFromPassword([]byte("password123"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	if _, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID: "cust-signin", Email: "test@example.com", DisplayName: "", PictureUri: "", PasswordHash: string(hash),
	}); err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}

	var signinToken string

	t.Run("SignInWithEmail_valid", func(t *testing.T) {
		token, err := svc.SignInWithEmail(ctx, "test@example.com", "password123")
		if err != nil {
			t.Fatalf("SignInWithEmail: %v", err)
		}
		if token == "" {
			t.Fatal("expected non-empty token")
		}
		signinToken = token
	})

	t.Run("SignInWithEmail_wrongPassword", func(t *testing.T) {
		if _, err := svc.SignInWithEmail(ctx, "test@example.com", "wrong"); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("err = %v, want ErrInvalidCredentials", err)
		}
	})

	t.Run("JWT_structure", func(t *testing.T) {
		parsed, err := jwt.Parse(signinToken, func(tok *jwt.Token) (any, error) {
			if _, ok := tok.Method.(*jwt.SigningMethodHMAC); !ok {
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
// fire-and-forget silent-drop path in RequestMagicLink without involving NATS.
type failingPublisher struct{}

func (failingPublisher) Publish(_ context.Context, _ string, _ []byte) error {
	return errors.New("simulated publish failure")
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
