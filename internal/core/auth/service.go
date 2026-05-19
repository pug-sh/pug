package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	"github.com/pug-sh/pug/internal/deps/nats"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	emailworkerv1 "github.com/pug-sh/pug/internal/gen/proto/workers/email/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
	"github.com/rs/xid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"golang.org/x/crypto/bcrypt"
	"google.golang.org/protobuf/proto"
)

// emailPublishFailureCounter is incremented whenever an email job is created
// (token committed, customer/invitation row committed) but the subsequent
// NATS publish errors. Operators should alert on a non-zero rate: it means
// users are seeing "check your email" responses for emails that were never
// queued. The {kind} attribute lets ops bucket failures by payload type.
var emailPublishFailureCounter metric.Int64Counter

func init() {
	meter := otel.Meter("github.com/pug-sh/pug/internal/core/auth")
	// Panic on init failure: without this counter, every subsequent
	// Add() is a no-op and the only alarm for silent email drops is gone.
	// Fail loud at startup rather than silently lose the alerting signal.
	c, err := meter.Int64Counter(
		"emails.publish_failure_total",
		metric.WithDescription("Email jobs whose tx committed but NATS publish failed; indicates user-visible silent drops."),
	)
	if err != nil {
		panic("auth: failed to register emails.publish_failure_total counter: " + err.Error())
	}
	emailPublishFailureCounter = c
}

var (
	ErrEmailAlreadyExists = errors.New("user with this email already exists")
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrInvalidToken       = errors.New("invalid or expired token")
	// ErrPasswordTooLong is returned when a password exceeds bcrypt's 72-byte
	// input limit. Proto validation enforces the same limit at the
	// interceptor, so reaching this sentinel from the service is rare in
	// production (direct calls from tests / future non-RPC callers).
	ErrPasswordTooLong = errors.New("password is too long")
)

const (
	aud = "pug/dashboard"
	iss = "pug/auth"

	verifyEmailPurpose = "verify_email"
	resetPasswordCause = "reset_password"

	verifyEmailTTL   = 24 * time.Hour
	resetPasswordTTL = 2 * time.Hour
)

type JobPublisher interface {
	Publish(ctx context.Context, subject string, data []byte) error
}

type Service struct {
	read      *dbread.Queries
	write     *dbwrite.Queries
	pgW       *pgxpool.Pool
	jwtKey    []byte
	publisher JobPublisher
}

func NewService(pgRO *pgxpool.Pool, pgW *pgxpool.Pool, jwtKey []byte, publisher JobPublisher) *Service {
	return &Service{
		read:      dbread.New(pgRO),
		write:     dbwrite.New(pgW),
		pgW:       pgW,
		jwtKey:    jwtKey,
		publisher: publisher,
	}
}

func (s *Service) SignUpWithEmail(ctx context.Context, email, password string) (string, error) {
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		if errors.Is(err, bcrypt.ErrPasswordTooLong) {
			return "", ErrPasswordTooLong
		}
		slog.ErrorContext(ctx, "failed to hash password", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return "", err
	}

	customerID := xid.New().String()
	verifyToken, err := newActionToken()
	if err != nil {
		slog.ErrorContext(ctx, "failed to generate verify token", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return "", err
	}
	verifyTokenHash := hashToken(verifyToken)

	tx, err := s.pgW.Begin(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to begin transaction", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return "", err
	}
	defer func() {
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			slog.ErrorContext(ctx, "failed rolling back signup transaction", slogx.Error(err))
			telemetry.RecordError(ctx, err)
		}
	}()

	w := dbwrite.New(tx)

	if _, err = w.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID:           customerID,
		Email:        email,
		DisplayName:  "",
		PictureUri:   "",
		PasswordHash: string(passwordHash),
	}); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgerrcode.UniqueViolation {
			return "", ErrEmailAlreadyExists
		}
		slog.ErrorContext(ctx, "failed to create customer", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return "", err
	}

	if _, err = w.CreateEmailActionToken(ctx, dbwrite.CreateEmailActionTokenParams{
		ID:              xid.New().String(),
		CustomerID:      postgres.NewOptionalText(customerID),
		Email:           email,
		Purpose:         verifyEmailPurpose,
		TokenHash:       verifyTokenHash,
		OrgInvitationID: postgres.NewOptionalText(""),
		ExpiresAt:       postgres.NewTimestamptz(time.Now().Add(verifyEmailTTL)),
	}); err != nil {
		slog.ErrorContext(ctx, "failed to create email verification token", slogx.Error(err), slog.String("customer_id", customerID))
		telemetry.RecordError(ctx, err)
		return "", err
	}

	if _, err = coreorgs.CreateOrgWithDefaultsInTx(ctx, w, customerID, "default"); err != nil {
		return "", err
	}

	if err = tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "failed to commit signup transaction", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return "", err
	}

	s.publishEmailJob(ctx, &emailworkerv1.EmailJob{
		Payload: &emailworkerv1.EmailJob_SignupVerifyWelcome{
			SignupVerifyWelcome: &emailworkerv1.SignUpVerifyWelcomePayload{
				Email: proto.String(email),
				Token: proto.String(verifyToken),
			},
		},
	})

	token, err := s.generateJWT(customerID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to generate JWT", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return "", err
	}

	return token, nil
}

func (s *Service) SignInWithEmail(ctx context.Context, email, password string) (string, error) {
	customer, err := s.read.GetCustomerByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrInvalidCredentials
		}
		slog.ErrorContext(ctx, "failed to get customer by email", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return "", err
	}

	err = bcrypt.CompareHashAndPassword([]byte(customer.PasswordHash), []byte(password))
	if err != nil {
		if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
			return "", ErrInvalidCredentials
		}
		slog.ErrorContext(ctx, "failed to compare password hash", slogx.Error(err), slog.String("customer_id", customer.ID))
		telemetry.RecordError(ctx, err)
		return "", err
	}

	token, err := s.generateJWT(customer.ID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to generate JWT", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return "", err
	}

	return token, nil
}

func (s *Service) VerifyEmail(ctx context.Context, token string) error {
	tx, err := s.pgW.Begin(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to begin verify email transaction", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return err
	}
	defer func() {
		if rollbackErr := tx.Rollback(ctx); rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			slog.ErrorContext(ctx, "failed rolling back verify email transaction", slogx.Error(rollbackErr))
			telemetry.RecordError(ctx, rollbackErr)
		}
	}()

	r := dbread.New(tx)
	w := dbwrite.New(tx)

	emailToken, err := r.GetValidEmailActionTokenByHashAndPurpose(ctx, dbread.GetValidEmailActionTokenByHashAndPurposeParams{
		TokenHash: hashToken(token),
		Purpose:   verifyEmailPurpose,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidToken
		}
		slog.ErrorContext(ctx, "failed to load verify email token", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return err
	}
	if !emailToken.CustomerID.Valid {
		return ErrInvalidToken
	}

	if _, err := w.ConsumeEmailActionToken(ctx, emailToken.ID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidToken
		}
		slog.ErrorContext(ctx, "failed to consume verify email token", slogx.Error(err), slog.String("token_id", emailToken.ID))
		telemetry.RecordError(ctx, err)
		return err
	}

	if _, err := w.MarkCustomerEmailVerified(ctx, emailToken.CustomerID.String); err != nil {
		slog.ErrorContext(ctx, "failed to mark customer email verified", slogx.Error(err), slog.String("customer_id", emailToken.CustomerID.String))
		telemetry.RecordError(ctx, err)
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "failed to commit verify email transaction", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return err
	}

	return nil
}

func (s *Service) RequestPasswordReset(ctx context.Context, email string) error {
	customer, err := s.read.GetCustomerByEmailOptional(ctx, email)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		slog.ErrorContext(ctx, "failed to lookup password reset customer", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return err
	}

	resetToken, err := newActionToken()
	if err != nil {
		slog.ErrorContext(ctx, "failed to generate reset token", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return err
	}
	if err := s.issueActionTokenAndPublish(ctx, issueActionTokenInput{
		CustomerID: customer.ID,
		Email:      customer.Email,
		Purpose:    resetPasswordCause,
		TTL:        resetPasswordTTL,
		RawToken:   resetToken,
		Job: &emailworkerv1.EmailJob{
			Payload: &emailworkerv1.EmailJob_PasswordReset{
				PasswordReset: &emailworkerv1.PasswordResetPayload{
					Email: proto.String(customer.Email),
					Token: proto.String(resetToken),
				},
			},
		},
	}); err != nil {
		return err
	}

	return nil
}

func (s *Service) ResetPassword(ctx context.Context, token, password string) error {
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		if errors.Is(err, bcrypt.ErrPasswordTooLong) {
			return ErrPasswordTooLong
		}
		slog.ErrorContext(ctx, "failed to hash reset password", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return err
	}

	tx, err := s.pgW.Begin(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to begin reset password transaction", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return err
	}
	defer func() {
		if rollbackErr := tx.Rollback(ctx); rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			slog.ErrorContext(ctx, "failed rolling back reset password transaction", slogx.Error(rollbackErr))
			telemetry.RecordError(ctx, rollbackErr)
		}
	}()

	r := dbread.New(tx)
	w := dbwrite.New(tx)

	emailToken, err := r.GetValidEmailActionTokenByHashAndPurpose(ctx, dbread.GetValidEmailActionTokenByHashAndPurposeParams{
		TokenHash: hashToken(token),
		Purpose:   resetPasswordCause,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidToken
		}
		slog.ErrorContext(ctx, "failed to lookup reset password token", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return err
	}
	if !emailToken.CustomerID.Valid {
		return ErrInvalidToken
	}

	if _, err := w.ConsumeEmailActionToken(ctx, emailToken.ID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidToken
		}
		slog.ErrorContext(ctx, "failed to consume password reset token", slogx.Error(err), slog.String("token_id", emailToken.ID))
		telemetry.RecordError(ctx, err)
		return err
	}

	if _, err := w.UpdateCustomerPasswordHash(ctx, dbwrite.UpdateCustomerPasswordHashParams{
		ID:           emailToken.CustomerID.String,
		PasswordHash: string(passwordHash),
	}); err != nil {
		slog.ErrorContext(ctx, "failed to update customer password hash", slogx.Error(err), slog.String("customer_id", emailToken.CustomerID.String))
		telemetry.RecordError(ctx, err)
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "failed to commit reset password transaction", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return err
	}

	return nil
}

func (s *Service) ResendVerificationEmail(ctx context.Context, email string) error {
	customer, err := s.read.GetCustomerByEmailOptional(ctx, email)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		slog.ErrorContext(ctx, "failed to lookup verification resend customer", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return err
	}
	if customer.EmailVerifiedAt.Valid {
		return nil
	}

	verifyToken, err := newActionToken()
	if err != nil {
		slog.ErrorContext(ctx, "failed to generate verify resend token", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return err
	}
	if err := s.issueActionTokenAndPublish(ctx, issueActionTokenInput{
		CustomerID: customer.ID,
		Email:      customer.Email,
		Purpose:    verifyEmailPurpose,
		TTL:        verifyEmailTTL,
		RawToken:   verifyToken,
		Job: &emailworkerv1.EmailJob{
			Payload: &emailworkerv1.EmailJob_VerificationResend{
				VerificationResend: &emailworkerv1.VerificationResendPayload{
					Email: proto.String(customer.Email),
					Token: proto.String(verifyToken),
				},
			},
		},
	}); err != nil {
		return err
	}

	return nil
}

type issueActionTokenInput struct {
	CustomerID string
	Email      string
	Purpose    string
	TTL        time.Duration
	RawToken   string
	Job        *emailworkerv1.EmailJob
}

func (s *Service) issueActionTokenAndPublish(ctx context.Context, input issueActionTokenInput) error {
	tx, err := s.pgW.Begin(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to begin issue action token transaction", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return err
	}
	defer func() {
		if rollbackErr := tx.Rollback(ctx); rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			slog.ErrorContext(ctx, "failed rolling back issue action token transaction", slogx.Error(rollbackErr))
			telemetry.RecordError(ctx, rollbackErr)
		}
	}()

	w := dbwrite.New(tx)
	if input.CustomerID != "" {
		if _, err := w.InvalidateActiveEmailActionTokensByCustomer(ctx, dbwrite.InvalidateActiveEmailActionTokensByCustomerParams{
			CustomerID: postgres.NewOptionalText(input.CustomerID),
			Purpose:    input.Purpose,
		}); err != nil {
			slog.ErrorContext(ctx, "failed to invalidate customer email action tokens", slogx.Error(err), slog.String("customer_id", input.CustomerID), slog.String("purpose", input.Purpose))
			telemetry.RecordError(ctx, err)
			return err
		}
	}
	if _, err := w.InvalidateActiveEmailActionTokensByEmail(ctx, dbwrite.InvalidateActiveEmailActionTokensByEmailParams{
		Email:   input.Email,
		Purpose: input.Purpose,
	}); err != nil {
		slog.ErrorContext(ctx, "failed to invalidate email action tokens by email", slogx.Error(err), slog.String("email", input.Email), slog.String("purpose", input.Purpose))
		telemetry.RecordError(ctx, err)
		return err
	}
	if _, err := w.CreateEmailActionToken(ctx, dbwrite.CreateEmailActionTokenParams{
		ID:              xid.New().String(),
		CustomerID:      postgres.NewOptionalText(input.CustomerID),
		Email:           input.Email,
		Purpose:         input.Purpose,
		TokenHash:       hashToken(input.RawToken),
		OrgInvitationID: postgres.NewOptionalText(""),
		ExpiresAt:       postgres.NewTimestamptz(time.Now().Add(input.TTL)),
	}); err != nil {
		slog.ErrorContext(ctx, "failed to create email action token", slogx.Error(err), slog.String("email", input.Email), slog.String("purpose", input.Purpose))
		telemetry.RecordError(ctx, err)
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "failed to commit issue action token transaction", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return err
	}

	s.publishEmailJob(ctx, input.Job)
	return nil
}

func (s *Service) publishEmailJob(ctx context.Context, job *emailworkerv1.EmailJob) {
	if s.publisher == nil || job == nil {
		return
	}
	data, err := proto.Marshal(job)
	if err != nil {
		slog.ErrorContext(ctx, "failed to marshal email job", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		emailPublishFailureCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", payloadKindFromJob(ctx, job))))
		return
	}
	if err := s.publisher.Publish(ctx, nats.MiscEmailJobsSubject, data); err != nil {
		slog.ErrorContext(ctx, "failed to publish email job", slogx.Error(err), slog.String("subject", nats.MiscEmailJobsSubject))
		telemetry.RecordError(ctx, err)
		emailPublishFailureCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", payloadKindFromJob(ctx, job))))
	}
}

func payloadKindFromJob(ctx context.Context, job *emailworkerv1.EmailJob) string {
	switch job.GetPayload().(type) {
	case *emailworkerv1.EmailJob_SignupVerifyWelcome:
		return "signup_verify_welcome"
	case *emailworkerv1.EmailJob_PasswordReset:
		return "password_reset"
	case *emailworkerv1.EmailJob_VerificationResend:
		return "verification_resend"
	case *emailworkerv1.EmailJob_OrgMemberInvite:
		return "org_member_invite"
	default:
		// Unknown payload type means the EmailJob proto grew a new oneof case
		// that this switch hasn't been updated to handle. The counter still
		// records the failure under kind="unknown", but the warn log is the
		// signal to add the missing case here.
		slog.WarnContext(ctx, "unknown email job payload kind; counter falling back to 'unknown'",
			slog.String("payload_type", fmt.Sprintf("%T", job.GetPayload())))
		return "unknown"
	}
}

type AdditionalClaims struct {
	Email string `json:"email"`
}

type UserClaims struct {
	jwt.RegisteredClaims
	AdditionalClaims
}

func (s *Service) generateJWT(id string) (string, error) {
	// todo - reduce expiry time
	now := time.Now()
	standardClaims := jwt.RegisteredClaims{
		Audience:  jwt.ClaimStrings{aud},
		ExpiresAt: jwt.NewNumericDate(now.Add(90 * 24 * time.Hour)), // expire in 90 days
		ID:        xid.New().String(),
		IssuedAt:  jwt.NewNumericDate(now),
		Issuer:    iss,
		Subject:   id,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, standardClaims)

	tokenString, err := token.SignedString(s.jwtKey)
	if err != nil {
		return "", err
	}

	return tokenString, nil
}

// newActionToken returns a 32-byte cryptographically-random token, hex-encoded
// (64 chars). This is the sole secret an attacker would need to redeem a
// password-reset or email-verification, so it must come from crypto/rand —
// not a monotonic ID generator like xid.
func newActionToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
