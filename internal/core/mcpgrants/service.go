// Package mcpgrants makes pug an OAuth authorization server for its own /mcp. A
// grant lets one client read every project in the orgs its person joined before
// approving and hasn't left since; its token pairs are rows in refresh_tokens.
package mcpgrants

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pug-sh/pug/internal/cimd"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/slogx"
)

const (
	AccessTokenPrefix  = "mcp_at_"
	refreshTokenPrefix = "mcp_rt_"

	// Scope is the one scope, granted whatever a client asks for.
	Scope = "read"

	AccessTokenTTL  = time.Hour
	codeTTL         = 5 * time.Minute
	refreshTokenTTL = 90 * 24 * time.Hour
	// reuseGrace covers a rotation whose answer never reached the client.
	reuseGrace = time.Minute
)

var (
	ErrClientInvalid     = errors.New("client_id or its metadata document is invalid")
	ErrClientUnavailable = errors.New("client metadata document could not be loaded")
	ErrRedirectNotListed = errors.New("redirect_uri is not in the client's metadata document")
	ErrSessionEnded      = errors.New("the approving session has ended")
	ErrNoOrg             = errors.New("the approving person belongs to no org")
	ErrProjectNotFound   = errors.New("project not found")
	ErrInvalidToken      = errors.New("invalid or expired access token")
	ErrGrantNotFound     = errors.New("grant not found")
)

// An Error is an OAuth error the client sees (RFC 6749 section 5.2).
type Error struct{ code string }

func (e *Error) Error() string { return e.code }
func (e *Error) Code() string  { return e.code }

var (
	ErrAccessDenied            = &Error{"access_denied"}
	ErrInvalidClient           = &Error{"invalid_client"}
	ErrInvalidRequest          = &Error{"invalid_request"}
	ErrInvalidGrant            = &Error{"invalid_grant"}
	ErrInvalidTarget           = &Error{"invalid_target"}
	ErrUnsupportedGrantType    = &Error{"unsupported_grant_type"}
	ErrUnsupportedResponseType = &Error{"unsupported_response_type"}
)

type Service struct {
	pgW       *pgxpool.Pool
	documents DocumentFetcher
	issuer    string
}

// DocumentFetcher is the slice of *cimd.Client the service uses.
type DocumentFetcher interface {
	Fetch(ctx context.Context, clientID string) (cimd.Document, error)
}

// NewService serves issuer's /mcp; issuer is an origin such as https://api.pugs.dev.
// Reads use the writer pool, because a token is used the moment it is issued.
func NewService(pgW *pgxpool.Pool, documents DocumentFetcher, issuer string) *Service {
	return &Service{pgW: pgW, documents: documents, issuer: issuer}
}

func (s *Service) Issuer() string { return s.issuer }

// allowsResource reports whether r is absent or names this /mcp, as typed with or
// without the slash.
func (s *Service) allowsResource(r string) bool {
	return r == "" || r == s.issuer+"/mcp" || r == s.issuer+"/mcp/"
}

// rand.Read never returns an error; it crashes the program instead.
func newToken(prefix string) string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func recordServiceError(ctx context.Context, msg string, err error, attrs ...any) error {
	slog.ErrorContext(ctx, msg, append([]any{slogx.Error(err)}, attrs...)...)
	telemetry.RecordError(ctx, err)
	return err
}
