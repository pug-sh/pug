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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pug-sh/pug/internal/core/emailaction"
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
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrInvalidToken       = errors.New("invalid or expired token")
)

const (
	aud = "pug/dashboard"
	iss = "pug/auth"

	magicLinkTTL = 15 * time.Minute
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

	if customer.PasswordHash == "" {
		// Passwordless (magic-link) account — no password set. Treat as invalid
		// credentials rather than letting bcrypt fail on an empty hash (which
		// returns ErrHashTooShort, not ErrMismatchedHashAndPassword).
		return "", ErrInvalidCredentials
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

// RequestMagicLink issues and emails a single-use magic link for the given
// email. It always succeeds regardless of whether an account exists (no
// account-existence oracle); CompleteMagicLink creates the account on first use.
func (s *Service) RequestMagicLink(ctx context.Context, email string) error {
	customerID := ""
	tokenEmail := email
	if customer, err := s.read.GetCustomerByEmailOptional(ctx, email); err == nil {
		customerID = customer.ID
		tokenEmail = customer.Email
	} else if !errors.Is(err, pgx.ErrNoRows) {
		slog.ErrorContext(ctx, "failed to look up magic-link customer", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return err
	}

	rawToken, err := newActionToken()
	if err != nil {
		slog.ErrorContext(ctx, "failed to generate magic-link token", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return err
	}
	return s.issueActionTokenAndPublish(ctx, issueActionTokenInput{
		CustomerID: customerID,
		Email:      tokenEmail,
		Purpose:    emailaction.PurposeMagicLink.String(),
		TTL:        magicLinkTTL,
		RawToken:   rawToken,
		Job: &emailworkerv1.EmailJob{
			Payload: &emailworkerv1.EmailJob_MagicLink{
				MagicLink: &emailworkerv1.MagicLinkPayload{
					Email: proto.String(tokenEmail),
					Token: proto.String(rawToken),
				},
			},
		},
	})
}

// CompleteMagicLink validates a single-use magic-link token, ensures an account
// exists for the token's email (creating a passwordless one on first use), marks
// the email verified, consumes the token, and returns a session JWT. It ignores
// any caller session — the token alone decides identity.
//
// When the token carries an org_invitation_id (invite branch), the new or
// existing account joins the invited org with its role; no default org is
// created. When org_invitation_id is NULL (plain branch), a newly-created
// account receives a default org + project.
func (s *Service) CompleteMagicLink(ctx context.Context, token string) (string, error) {
	tx, err := s.pgW.Begin(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to begin magic-link transaction", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return "", err
	}
	defer func() {
		if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			slog.ErrorContext(ctx, "failed rolling back magic-link transaction", slogx.Error(rbErr))
			telemetry.RecordError(ctx, rbErr)
		}
	}()

	r := dbread.New(tx)
	w := dbwrite.New(tx)

	// FOR UPDATE locks the token row so concurrent redemptions of the same token
	// (double-click, prefetch + click) serialize on it: the first to commit
	// consumes the token, and the rest re-read zero rows and fall into the
	// ErrInvalidToken path below rather than racing ahead to a duplicate-customer
	// unique violation that would surface as CodeInternal.
	emailToken, err := w.GetValidEmailActionTokenByHashForUpdate(ctx, hashToken(token))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrInvalidToken
		}
		slog.ErrorContext(ctx, "failed to look up magic-link token", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return "", err
	}

	// token_hash is unique, so the lookup above is purpose-agnostic. Gate on
	// purpose here so this endpoint only redeems login and invite tokens — a
	// token minted for any other purpose (e.g. a future password-change flow)
	// is rejected rather than silently consumed as a login.
	var isInvite bool
	switch emailaction.Purpose(emailToken.Purpose) {
	case emailaction.PurposeMagicLink:
		isInvite = false
	case emailaction.PurposeOrgInvite:
		isInvite = true
	default:
		return "", ErrInvalidToken
	}

	customerID := ""
	createdNew := false
	if existing, err := r.GetCustomerByEmailOptional(ctx, emailToken.Email); err == nil {
		customerID = existing.ID
	} else if errors.Is(err, pgx.ErrNoRows) {
		customerID = xid.New().String()
		createdNew = true
		if _, err := w.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
			ID: customerID, Email: emailToken.Email, DisplayName: "", PictureUri: "", PasswordHash: "",
		}); err != nil {
			slog.ErrorContext(ctx, "failed to create magic-link customer", slogx.Error(err))
			telemetry.RecordError(ctx, err)
			return "", err
		}
	} else {
		slog.ErrorContext(ctx, "failed to look up magic-link customer", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return "", err
	}

	if isInvite {
		// Invited user joins the invitation's org with its role; no default org.
		if err := coreorgs.ApplyInviteAcceptanceInTx(ctx, w, emailToken.OrgInvitationID.String, customerID); err != nil {
			switch {
			case errors.Is(err, coreorgs.ErrAlreadyMember):
				// Idempotent — already in the org; just sign in.
			case errors.Is(err, coreorgs.ErrInviteNotFound),
				errors.Is(err, coreorgs.ErrInviteNotPending),
				errors.Is(err, coreorgs.ErrInviteExpired):
				return "", ErrInvalidToken
			default:
				return "", err // logged + recorded at source in coreorgs
			}
		}
	} else if createdNew {
		// Plain passwordless signup: give the new account a default org + project.
		if _, err := coreorgs.CreateOrgWithDefaultsInTx(ctx, w, customerID, "default"); err != nil {
			slog.ErrorContext(ctx, "failed to create default org for magic-link user", slogx.Error(err), slog.String("customer_id", customerID))
			return "", err
		}
	}

	if _, err := w.ConsumeEmailActionToken(ctx, emailToken.ID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrInvalidToken
		}
		slog.ErrorContext(ctx, "failed to consume magic-link token", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return "", err
	}
	if _, err := w.MarkCustomerEmailVerified(ctx, customerID); err != nil {
		slog.ErrorContext(ctx, "failed to mark email verified on magic-link", slogx.Error(err), slog.String("customer_id", customerID))
		telemetry.RecordError(ctx, err)
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "failed to commit magic-link transaction", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return "", err
	}

	jwtToken, err := s.generateJWT(customerID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to generate JWT on magic-link", slogx.Error(err), slog.String("customer_id", customerID))
		telemetry.RecordError(ctx, err)
		return "", err
	}
	return jwtToken, nil
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
	case *emailworkerv1.EmailJob_OrgMemberInvite:
		return "org_member_invite"
	case *emailworkerv1.EmailJob_MagicLink:
		return "magic_link"
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
// magic link, so it must come from crypto/rand —
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
