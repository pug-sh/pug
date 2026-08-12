package ai

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"connectrpc.com/authn"
	"connectrpc.com/connect"
	"github.com/golang-jwt/jwt/v5"

	pogrpc "github.com/pug-sh/pug/internal/app/server/rpc"
	coreauth "github.com/pug-sh/pug/internal/core/auth"
)

// Caller is the per-request identity the auth boundary stashes for the
// handler: the raw JWT to forward downstream and the project scope. No
// customer row — this service never touches the database.
type Caller struct {
	JWT       string
	ProjectID string
}

func unauthenticated(msg string) error {
	return connect.NewError(connect.CodeUnauthenticated, errors.New(msg))
}

// WithAssistantAuth is the ai role's auth boundary: the DB-free core of the
// server's WithJWTAuth (bearer + HS256 + iss/aud/exp pins) plus a required
// x-project-id. It is a SPEND GATE, not the security boundary — it rejects
// junk before any model tokens are burned (a turn with no tool calls would
// otherwise never be rejected at all), but a revoked-yet-unexpired token still
// passes here and is enforced by the insights callback, which re-runs full
// auth (including org membership for the named project) on every tool call.
func WithAssistantAuth(jwtKey []byte) authn.AuthFunc {
	return func(ctx context.Context, req *http.Request) (any, error) {
		authHeader := req.Header.Get("Authorization")
		if authHeader == "" {
			return nil, unauthenticated("Authorization header not present")
		}
		if !strings.HasPrefix(authHeader, "Bearer ") {
			return nil, unauthenticated("Authorization header must start with Bearer")
		}
		token := strings.TrimPrefix(authHeader, "Bearer ")
		if token == "" {
			return nil, unauthenticated("Bearer token is empty")
		}

		parsedJWT, err := jwt.Parse(token, func(t *jwt.Token) (any, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, authn.Errorf("unexpected signing method: %v", t.Header["alg"])
			}
			return jwtKey, nil
		},
			// Same pins as the server's WithJWTAuth: reject a forged alg header
			// before the keyfunc runs, and require the aud/iss/exp our issuer
			// sets so a token minted for another audience or signed without an
			// expiry is rejected rather than silently accepted.
			jwt.WithValidMethods([]string{"HS256"}),
			jwt.WithIssuer(coreauth.Issuer),
			jwt.WithAudience(coreauth.Audience),
			jwt.WithExpirationRequired(),
		)
		if err != nil || !parsedJWT.Valid {
			return nil, unauthenticated("invalid authorization")
		}

		projectID := req.Header.Get(pogrpc.HeaderProjectID)
		if projectID == "" {
			return nil, unauthenticated("x-project-id header is required")
		}

		return &Caller{JWT: token, ProjectID: projectID}, nil
	}
}
