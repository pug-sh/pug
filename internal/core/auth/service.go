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
	coreoauth "github.com/pug-sh/pug/internal/core/auth/oauth"
	"github.com/pug-sh/pug/internal/core/emailaction"
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
	// Audience and Issuer are baked into every session JWT by generateJWT and
	// verified by the auth middleware (rpc.WithJWTAuth). Exported so the issuing
	// and verifying sides share one source of truth and cannot drift.
	Audience = "pug/dashboard"
	Issuer   = "pug/auth"

	magicLinkTTL = 15 * time.Minute

	// accessTokenTTL is the lifetime of the session JWT. Kept short so a leaked
	// access token is useful only briefly; clients silently exchange the
	// long-lived refresh token for a new pair via RefreshSession.
	accessTokenTTL = 1 * time.Hour
	// refreshTokenTTL is the sliding lifetime of a refresh token. Every refresh
	// issues a fresh one, so a user active at least once per window stays signed
	// in indefinitely; one fully idle for the whole window must sign in again.
	refreshTokenTTL = 90 * 24 * time.Hour
)

// Session is the token pair returned by every sign-in path (password, magic link,
// OAuth) and by RefreshSession. AccessToken is the short-lived JWT sent as the
// bearer; RefreshToken is the long-lived opaque secret exchanged for a new pair.
type Session struct {
	AccessToken  string
	RefreshToken string
}

type JobPublisher interface {
	Publish(ctx context.Context, subject string, data []byte) error
}

type Service struct {
	read      *dbread.Queries
	write     *dbwrite.Queries
	pgW       *pgxpool.Pool
	jwtKey    []byte
	publisher JobPublisher
	oauth     *coreoauth.Service
	// demoEnabled gates DemoSignIn, the credential-less viewer login. It mirrors
	// PUG_DEMO_ENABLED so the public demo login is only mintable on a demo
	// deployment; everywhere else DemoSignIn returns ErrDemoUnavailable.
	demoEnabled bool
}

func NewService(ctx context.Context, pgRO *pgxpool.Pool, pgW *pgxpool.Pool, jwtKey []byte, publisher JobPublisher, oauthCfg coreoauth.Config, demoEnabled bool) (*Service, error) {
	registry, err := coreoauth.NewRegistryFromConfig(ctx, oauthCfg)
	if err != nil {
		return nil, err
	}
	oauthSvc := coreoauth.NewService(oauthCfg, registry)

	return &Service{
		read:        dbread.New(pgRO),
		write:       dbwrite.New(pgW),
		pgW:         pgW,
		jwtKey:      jwtKey,
		publisher:   publisher,
		oauth:       oauthSvc,
		demoEnabled: demoEnabled,
	}, nil
}

func (s *Service) SignInWithEmail(ctx context.Context, email, password string) (Session, error) {
	customer, err := s.read.GetCustomerByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Session{}, ErrInvalidCredentials
		}
		slog.ErrorContext(ctx, "failed to get customer by email", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return Session{}, err
	}

	if customer.PasswordHash == "" {
		// Passwordless (magic-link) account — no password set. Treat as invalid
		// credentials rather than letting bcrypt fail on an empty hash (which
		// returns ErrHashTooShort, not ErrMismatchedHashAndPassword).
		return Session{}, ErrInvalidCredentials
	}

	err = bcrypt.CompareHashAndPassword([]byte(customer.PasswordHash), []byte(password))
	if err != nil {
		if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
			return Session{}, ErrInvalidCredentials
		}
		slog.ErrorContext(ctx, "failed to compare password hash", slogx.Error(err), slog.String("customer_id", customer.ID))
		telemetry.RecordError(ctx, err)
		return Session{}, err
	}

	return s.issueSession(ctx, customer.ID)
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
func (s *Service) CompleteMagicLink(ctx context.Context, token, reportingTimezone string) (Session, error) {
	tx, err := s.pgW.Begin(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to begin magic-link transaction", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return Session{}, err
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
			return Session{}, ErrInvalidToken
		}
		slog.ErrorContext(ctx, "failed to look up magic-link token", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return Session{}, err
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
		return Session{}, ErrInvalidToken
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
			return Session{}, err
		}
	} else {
		slog.ErrorContext(ctx, "failed to look up magic-link customer", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return Session{}, err
	}

	if isInvite {
		invite := &InviteContext{OrgInvitationID: emailToken.OrgInvitationID.String}
		if err := FinishSignup(ctx, w, customerID, createdNew, invite, reportingTimezone); err != nil {
			if !errors.Is(err, ErrInvalidToken) {
				slog.ErrorContext(ctx, "failed to apply org invite on magic-link", slogx.Error(err), slog.String("customer_id", customerID))
				telemetry.RecordError(ctx, err)
			}
			return Session{}, err
		}
	} else if createdNew {
		// Plain passwordless signup: give the new account a default org + project,
		// seeding the project's reporting timezone from the browser that completed
		// the link (coerced to UTC if malformed).
		if err := FinishSignup(ctx, w, customerID, true, nil, reportingTimezone); err != nil {
			slog.ErrorContext(ctx, "failed to create default org for magic-link user", slogx.Error(err), slog.String("customer_id", customerID))
			telemetry.RecordError(ctx, err)
			return Session{}, err
		}
	}

	if _, err := w.ConsumeEmailActionToken(ctx, emailToken.ID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Session{}, ErrInvalidToken
		}
		slog.ErrorContext(ctx, "failed to consume magic-link token", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return Session{}, err
	}
	if err := FinalizeVerifiedCustomer(ctx, w, customerID); err != nil {
		slog.ErrorContext(ctx, "failed to mark email verified on magic-link", slogx.Error(err), slog.String("customer_id", customerID))
		telemetry.RecordError(ctx, err)
		return Session{}, err
	}
	// Issue the refresh token inside the provisioning tx so a brand-new account and
	// its first session commit atomically: no customer is ever created without a
	// usable session, and a failed insert rolls the whole sign-up back.
	session, err := s.issueSessionTx(ctx, w, customerID)
	if err != nil {
		return Session{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "failed to commit magic-link transaction", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return Session{}, err
	}
	return session, nil
}

func (s *Service) CompleteOAuthSignIn(ctx context.Context, provider coreoauth.ProviderName, credential string) (Session, error) {
	ident, err := s.oauth.VerifyIdentity(ctx, provider, credential)
	if err != nil {
		// Client-input errors (ErrInvalidCredential / ErrUnverifiedEmail) are
		// mapped by the handler; unexpected verifier errors were already recorded
		// inside VerifyIdentity. Nothing to log or record here.
		return Session{}, err
	}

	var session Session
	_, _, err = coreoauth.WithIdentityTx(ctx, s.pgW, provider, ident, func(ctx context.Context, w *dbwrite.Queries, customerID string, createdNew bool) error {
		// OAuth sign-in carries no browser timezone; default the project to UTC.
		if err := FinishSignup(ctx, w, customerID, createdNew, nil, ""); err != nil {
			return err // coreorgs records this at its detect site; don't re-record.
		}
		if err := FinalizeVerifiedCustomer(ctx, w, customerID); err != nil {
			slog.ErrorContext(ctx, "failed to mark email verified on oauth sign-in", slogx.Error(err), slog.String("customer_id", customerID))
			telemetry.RecordError(ctx, err)
			return err
		}
		// Issue the session inside the identity tx so the refresh token commits
		// atomically with sign-up/link (same rationale as magic link).
		session, err = s.issueSessionTx(ctx, w, customerID)
		return err
	})
	if err != nil {
		// resolve.go records its own mechanism errors and the callback above
		// records FinalizeVerifiedCustomer / issueSessionTx; this wrapper only translates.
		return Session{}, err
	}
	return session, nil
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
	now := time.Now()
	standardClaims := jwt.RegisteredClaims{
		Audience:  jwt.ClaimStrings{Audience},
		ExpiresAt: jwt.NewNumericDate(now.Add(accessTokenTTL)),
		ID:        xid.New().String(),
		IssuedAt:  jwt.NewNumericDate(now),
		Issuer:    Issuer,
		Subject:   id,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, standardClaims)

	tokenString, err := token.SignedString(s.jwtKey)
	if err != nil {
		return "", err
	}

	return tokenString, nil
}

// issueSession mints an access JWT + a fresh-family refresh token using the
// pool-backed queries, as a standalone insert. For callers issuing a session
// outside any provisioning transaction — currently only password sign-in.
// (Magic link and OAuth issue inside their provisioning tx via issueSessionTx.)
func (s *Service) issueSession(ctx context.Context, customerID string) (Session, error) {
	return s.issueSessionTx(ctx, s.write, customerID)
}

// issueSessionTx is issueSession parameterized over a *dbwrite.Queries so it can
// run inside an existing transaction (magic link / OAuth sign-up) and commit the
// refresh token atomically with account provisioning. Each call starts a NEW
// rotation family.
func (s *Service) issueSessionTx(ctx context.Context, w *dbwrite.Queries, customerID string) (Session, error) {
	accessToken, err := s.generateJWT(customerID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to generate access token", slogx.Error(err), slog.String("customer_id", customerID))
		telemetry.RecordError(ctx, err)
		return Session{}, err
	}
	refreshToken, err := s.createRefreshToken(ctx, w, customerID, xid.New().String())
	if err != nil {
		return Session{}, err
	}
	return Session{AccessToken: accessToken, RefreshToken: refreshToken}, nil
}

// createRefreshToken generates an opaque refresh token, persists only its hash in
// the given family, and returns the plaintext (shown to the client once). The
// token is crypto-random for the same reason magic-link tokens are: it is the
// sole secret needed to mint sessions.
//
// TODO(refresh-token-pruning): refresh_tokens grows unbounded. Rotation inserts a
// new row on every refresh (~hourly per active user, given a 1h access TTL) and
// only marks the predecessor consumed_at — its expires_at stays ~90 days out, so
// nothing reaps it. Add a periodic prune (e.g. in the scheduler worker, which
// needs a write pool wired in) deleting rows that can no longer be presented:
//
//	delete from refresh_tokens
//	where expires_at < now()
//	   or (consumed_at is not null and consumed_at < now() - <reuse-grace>)
//	   or (revoked_at  is not null and revoked_at  < now() - <reuse-grace>);
//
// Keep a reuse-detection grace (~7d) on consumed/revoked rows so a replayed token
// still trips RevokeRefreshTokenFamily within that window; past it, a replay just
// reads as not-found (acceptable). Until this lands, the table only ever grows.
func (s *Service) createRefreshToken(ctx context.Context, w *dbwrite.Queries, customerID, familyID string) (string, error) {
	rawToken, err := newActionToken()
	if err != nil {
		slog.ErrorContext(ctx, "failed to generate refresh token", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return "", err
	}
	if _, err := w.CreateRefreshToken(ctx, dbwrite.CreateRefreshTokenParams{
		ID:         xid.New().String(),
		CustomerID: customerID,
		FamilyID:   familyID,
		TokenHash:  hashToken(rawToken),
		ExpiresAt:  postgres.NewTimestamptz(time.Now().Add(refreshTokenTTL)),
	}); err != nil {
		slog.ErrorContext(ctx, "failed to create refresh token", slogx.Error(err), slog.String("customer_id", customerID))
		telemetry.RecordError(ctx, err)
		return "", err
	}
	return rawToken, nil
}

// RefreshSession exchanges a valid refresh token for a new access+refresh pair,
// rotating (consuming) the presented token. It implements reuse-detection: a
// token that was ALREADY consumed being presented again means the chain was
// replayed (a leaked/stolen token, or a buggy client double-refreshing), so the
// whole rotation family is revoked and the request rejected — neither the
// attacker nor the legitimate client can keep refreshing, forcing a fresh
// sign-in. Returns ErrInvalidToken for unknown/expired/revoked/reused tokens.
func (s *Service) RefreshSession(ctx context.Context, refreshToken string) (Session, error) {
	tx, err := s.pgW.Begin(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to begin refresh transaction", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return Session{}, err
	}
	defer func() {
		if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			slog.ErrorContext(ctx, "failed rolling back refresh transaction", slogx.Error(rbErr))
			telemetry.RecordError(ctx, rbErr)
		}
	}()

	w := dbwrite.New(tx)

	// FOR UPDATE serializes concurrent refreshes of the same token. The lookup is
	// deliberately unfiltered (it returns consumed/revoked rows) so reuse can be
	// detected below rather than masked as "not found".
	row, err := w.GetRefreshTokenByHashForUpdate(ctx, hashToken(refreshToken))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Session{}, ErrInvalidToken
		}
		slog.ErrorContext(ctx, "failed to look up refresh token", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return Session{}, err
	}

	if row.ConsumedAt.Valid {
		// Reuse detected: kill the whole family and commit the revocation.
		if _, err := w.RevokeRefreshTokenFamily(ctx, row.FamilyID); err != nil {
			slog.ErrorContext(ctx, "failed to revoke reused refresh token family", slogx.Error(err), slog.String("customer_id", row.CustomerID))
			telemetry.RecordError(ctx, err)
			return Session{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			slog.ErrorContext(ctx, "failed to commit refresh token family revocation", slogx.Error(err))
			telemetry.RecordError(ctx, err)
			return Session{}, err
		}
		slog.WarnContext(ctx, "refresh token reuse detected; revoked family",
			slog.String("customer_id", row.CustomerID), slog.String("family_id", row.FamilyID))
		return Session{}, ErrInvalidToken
	}

	// Already revoked (sign-out or a prior family kill) or expired.
	if row.RevokedAt.Valid || !row.ExpiresAt.Time.After(time.Now()) {
		return Session{}, ErrInvalidToken
	}

	// Generate the access token before mutating state: a signing failure then
	// leaves the presented token unconsumed and the client can retry cleanly.
	accessToken, err := s.generateJWT(row.CustomerID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to generate access token on refresh", slogx.Error(err), slog.String("customer_id", row.CustomerID))
		telemetry.RecordError(ctx, err)
		return Session{}, err
	}

	if _, err := w.ConsumeRefreshToken(ctx, row.ID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Lost the race to a concurrent refresh of the same token.
			return Session{}, ErrInvalidToken
		}
		slog.ErrorContext(ctx, "failed to consume refresh token", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return Session{}, err
	}

	newRefresh, err := s.createRefreshToken(ctx, w, row.CustomerID, row.FamilyID)
	if err != nil {
		return Session{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "failed to commit refresh transaction", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return Session{}, err
	}

	return Session{AccessToken: accessToken, RefreshToken: newRefresh}, nil
}

// RevokeSession revokes the rotation family of the presented refresh token —
// the sign-out path. It is intentionally forgiving: an unknown, expired, or
// already-revoked token revokes nothing and still returns nil, so logging out
// with a stale token never surfaces an error to the client.
func (s *Service) RevokeSession(ctx context.Context, refreshToken string) error {
	if refreshToken == "" {
		return nil
	}
	if _, err := s.write.RevokeRefreshTokenFamilyByHash(ctx, hashToken(refreshToken)); err != nil {
		slog.ErrorContext(ctx, "failed to revoke refresh token family on sign-out", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return err
	}
	return nil
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
