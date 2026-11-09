package mcpgrants

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
	"github.com/rs/xid"
	"golang.org/x/oauth2"
)

// CodeExchange is a token request with grant_type authorization_code.
type CodeExchange struct {
	ClientID     string
	Code         string
	CodeVerifier string
	RedirectURI  string
	Resource     string
}

// TokenRefresh is a token request with grant_type refresh_token. ClientID and
// Resource are optional.
type TokenRefresh struct {
	ClientID     string
	RefreshToken string
	Resource     string
}

type Tokens struct {
	AccessToken  string
	RefreshToken string
}

// Access is who a live access token acts as. Each call names its project, which
// ResolveProject checks.
type Access struct {
	GrantID  string
	Customer dbread.Customer
}

// RFC 7636's code_verifier: 43 to 128 unreserved characters.
var verifierShape = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)

// ExchangeCode trades an approved code for the grant's first token pair.
func (s *Service) ExchangeCode(ctx context.Context, x CodeExchange) (Tokens, error) {
	switch {
	case x.Code == "", x.ClientID == "", x.CodeVerifier == "", x.RedirectURI == "":
		return Tokens{}, ErrInvalidRequest
	case !s.allowsResource(x.Resource):
		return Tokens{}, ErrInvalidTarget
	}
	tx, err := s.pgW.Begin(ctx)
	if err != nil {
		return Tokens{}, recordServiceError(ctx, "failed to begin mcp code exchange", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	w := dbwrite.New(tx)

	g, err := w.GetMCPGrantByCodeHashForUpdate(ctx, hashToken(x.Code))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Tokens{}, ErrInvalidGrant
	case err != nil:
		return Tokens{}, recordServiceError(ctx, "failed to look up mcp authorization code", err)
	case g.CodeUsedAt.Valid:
		// A second use means the code leaked.
		return Tokens{}, commitRevoke(ctx, tx, g.ID, errors.New("code already used"))
	case g.RevokeTime.Valid, !g.CodeExpiresAt.Time.After(time.Now()):
		return Tokens{}, fmt.Errorf("%w: code expired or revoked", ErrInvalidGrant)
	case g.ClientID != x.ClientID, g.RedirectUri != x.RedirectURI:
		return Tokens{}, fmt.Errorf("%w: client_id or redirect_uri differs from the approval", ErrInvalidGrant)
	case !verifierMatches(x.CodeVerifier, g.CodeChallenge):
		return Tokens{}, fmt.Errorf("%w: code_verifier doesn't match the challenge", ErrInvalidGrant)
	}
	if err := w.UseMCPGrantCode(ctx, g.ID); err != nil {
		return Tokens{}, recordServiceError(ctx, "failed to use mcp authorization code", err, slog.String("grant_id", g.ID))
	}
	return commitTokens(ctx, tx, g.ID, g.CustomerID)
}

// Refresh rotates a refresh token. A used one presented again within reuseGrace gets
// another pair; later, it revokes the grant.
func (s *Service) Refresh(ctx context.Context, r TokenRefresh) (Tokens, error) {
	switch {
	case r.RefreshToken == "":
		return Tokens{}, ErrInvalidRequest
	case !s.allowsResource(r.Resource):
		return Tokens{}, ErrInvalidTarget
	}
	tx, err := s.pgW.Begin(ctx)
	if err != nil {
		return Tokens{}, recordServiceError(ctx, "failed to begin mcp refresh", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	w := dbwrite.New(tx)

	row, err := w.GetMCPRefreshTokenByHashForUpdate(ctx, hashToken(r.RefreshToken))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Tokens{}, ErrInvalidGrant
	case err != nil:
		return Tokens{}, recordServiceError(ctx, "failed to look up mcp refresh token", err)
	case row.RevokeTime.Valid, !row.ExpiresAt.Time.After(time.Now()):
		return Tokens{}, fmt.Errorf("%w: refresh token expired or revoked", ErrInvalidGrant)
	case r.ClientID != "" && r.ClientID != row.ClientID:
		return Tokens{}, fmt.Errorf("%w: refresh token issued to another client", ErrInvalidGrant)
	case row.ConsumedAt.Valid && time.Since(row.ConsumedAt.Time) > reuseGrace:
		return Tokens{}, commitRevoke(ctx, tx, row.GrantID, errors.New("refresh token reused"))
	}
	if err := checkAccess(ctx, tx, row.GrantID, row.ProvenDomain.String); err != nil {
		return Tokens{}, err
	}
	if !row.ConsumedAt.Valid {
		if _, err := w.ConsumeRefreshToken(ctx, row.ID); err != nil {
			return Tokens{}, recordServiceError(ctx, "failed to consume mcp refresh token", err, slog.String("grant_id", row.GrantID))
		}
	}
	if err := w.MarkMCPGrantUsed(ctx, row.GrantID); err != nil {
		return Tokens{}, recordServiceError(ctx, "failed to mark mcp grant used", err, slog.String("grant_id", row.GrantID))
	}
	return commitTokens(ctx, tx, row.GrantID, row.CustomerID)
}

// ResolveAccessToken returns ErrInvalidToken unless the token is live and its
// grant still reaches an org. A nil Service is OAuth off, so no token is live.
func (s *Service) ResolveAccessToken(ctx context.Context, token string) (Access, error) {
	if s == nil {
		return Access{}, ErrInvalidToken
	}
	r := dbread.New(s.pgW)
	ids, err := r.GetMCPAccessByTokenHash(ctx, postgres.NewText(hashToken(token)))
	if err != nil {
		return Access{}, resolveError(ctx, err)
	}
	customer, err := r.GetCustomerByID(ctx, ids.CustomerID)
	if err != nil {
		return Access{}, resolveError(ctx, err)
	}
	return Access{GrantID: ids.GrantID, Customer: customer}, nil
}

// ResolveProject returns ErrProjectNotFound unless the grant reads the project: a
// live one in an org its person joined before the grant.
func (s *Service) ResolveProject(ctx context.Context, grantID, projectID string) (dbread.Project, error) {
	project, err := dbread.New(s.pgW).GetMCPGrantProject(ctx, dbread.GetMCPGrantProjectParams{GrantID: grantID, ProjectID: projectID})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return dbread.Project{}, ErrProjectNotFound
	case err != nil:
		return dbread.Project{}, recordServiceError(ctx, "failed to resolve mcp grant project", err,
			slog.String("grant_id", grantID), slog.String("project_id", projectID))
	}
	return project, nil
}

// resolveError reads a missing row as a dead token, though the customer may have
// gone only after the token check.
func resolveError(ctx context.Context, err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrInvalidToken
	}
	return recordServiceError(ctx, "failed to resolve mcp access token", err)
}

// checkAccess revokes a grant whose person lost access: they left every org they
// joined before it, or their domain requires an SSO the approval lacked.
func checkAccess(ctx context.Context, tx pgx.Tx, grantID, provenDomain string) error {
	r := dbread.New(tx)
	email, err := r.GetMCPGrantMemberEmail(ctx, grantID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return commitRevoke(ctx, tx, grantID, errors.New("the person left every org the grant reads"))
	case err != nil:
		return recordServiceError(ctx, "failed to check mcp grant membership", err, slog.String("grant_id", grantID))
	}
	err = coreorgs.CheckSignInInTx(ctx, r, email, provenDomain)
	if ssoErr, ok := errors.AsType[*coreorgs.SSORequiredError](err); ok {
		return commitRevoke(ctx, tx, grantID, ssoErr)
	}
	return err
}

// commitTokens mints the grant a token pair and commits.
func commitTokens(ctx context.Context, tx pgx.Tx, grantID, customerID string) (Tokens, error) {
	tokens := Tokens{AccessToken: newToken(AccessTokenPrefix), RefreshToken: newToken(refreshTokenPrefix)}
	now := time.Now()
	if err := dbwrite.New(tx).CreateMCPTokenPair(ctx, dbwrite.CreateMCPTokenPairParams{
		AccessExpiresAt: postgres.NewTimestamptz(now.Add(AccessTokenTTL)),
		AccessTokenHash: postgres.NewText(hashToken(tokens.AccessToken)),
		CustomerID:      customerID,
		ExpiresAt:       postgres.NewTimestamptz(now.Add(refreshTokenTTL)),
		FamilyID:        grantID,
		ID:              xid.New().String(),
		McpGrantID:      postgres.NewText(grantID),
		TokenHash:       hashToken(tokens.RefreshToken),
	}); err != nil {
		return Tokens{}, recordServiceError(ctx, "failed to create mcp token pair", err, slog.String("grant_id", grantID))
	}
	if err := tx.Commit(ctx); err != nil {
		return Tokens{}, recordServiceError(ctx, "failed to commit mcp token pair", err, slog.String("grant_id", grantID))
	}
	return tokens, nil
}

// commitRevoke revokes the grant and commits, so the revoke outlives the
// invalid_grant it returns.
func commitRevoke(ctx context.Context, tx pgx.Tx, grantID string, cause error) error {
	if err := dbwrite.New(tx).RevokeMCPGrant(ctx, grantID); err != nil {
		return recordServiceError(ctx, "failed to revoke mcp grant", err, slog.String("grant_id", grantID))
	}
	if err := tx.Commit(ctx); err != nil {
		return recordServiceError(ctx, "failed to commit mcp grant revocation", err, slog.String("grant_id", grantID))
	}
	slog.WarnContext(ctx, "revoked mcp grant", slog.String("grant_id", grantID), slogx.Error(cause))
	return fmt.Errorf("%w: %w", ErrInvalidGrant, cause)
}

// verifierMatches is PKCE's S256 check.
func verifierMatches(verifier, challenge string) bool {
	return verifierShape.MatchString(verifier) && oauth2.S256ChallengeFromVerifier(verifier) == challenge
}
