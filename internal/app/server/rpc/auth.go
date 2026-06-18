package rpc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"connectrpc.com/authn"
	"connectrpc.com/connect"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/pug-sh/pug/internal/apperr"
	coreauth "github.com/pug-sh/pug/internal/core/auth"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/slogx"
)

const (
	HeaderAPIKey     = "x-api-key"
	QueryAPIKey      = "api_key"
	HeaderProjectID  = "x-project-id"
	bearerPrefix     = "Bearer "
	publicKeyPrefix  = "pub_"
	privateKeyPrefix = "prv_"
)

// AuthType indicates which authentication method was used
type AuthType string

const (
	AuthTypeJWT        AuthType = "jwt"
	AuthTypePublicKey  AuthType = "pub_key"
	AuthTypePrivateKey AuthType = "priv_key"
)

// Principal represents the authenticated entity.
// Customer is set for JWT auth. Project is set for API key auth and JWT auth with x-project-id header.
type Principal struct {
	AuthType     AuthType
	Customer     *dbread.Customer
	Project      *dbread.Project
	JWTID        string // JWT ID (jti claim), set for JWT auth
	MaskedAPIKey string // Masked API key suffix (e.g. "...d456"), set for API key auth
}

// maskKey returns "...XXXX" showing only the last 4 characters of an API key,
// or "***" if the key is 4 characters or shorter.
func maskKey(key string) string {
	if len(key) > 4 {
		return "..." + key[len(key)-4:]
	}
	return "***"
}

// projectKeyLookup abstracts API key → project resolution for auth functions.
// Implementations must return pgx.ErrNoRows when no project matches the key, and must
// log + record non-ErrNoRows DB failures at source per CLAUDE.md (the auth boundary
// only translates errors).
//
// InvalidateProjectKeys is required as a structural marker so the sqlc-generated
// *dbread.Queries cannot accidentally satisfy this interface — its presence ensures
// callers wire a repo that respects the log-at-source contract.
type projectKeyLookup interface {
	GetProjectByPublicApiKey(ctx context.Context, key string) (dbread.Project, error)
	GetProjectByPrivateApiKey(ctx context.Context, key string) (dbread.Project, error)
	InvalidateProjectKeys(ctx context.Context, privateKey, publicKey string)
}

// unauthenticated builds an auth-rejection error carrying the standard error
// details (reason + correlation id). Auth runs in the authn middleware, OUTSIDE
// the Connect interceptor chain, so ErrorInterceptor never sees these errors;
// sourcing the details here keeps auth failures consistent with handler errors
// (every failure returns an error_id). authn serializes *connect.Error details
// via connect.NewErrorWriter, so the client receives them.
func unauthenticated(ctx context.Context, msg string) error {
	cerr := connect.NewError(connect.CodeUnauthenticated, errors.New(msg)) // apperr:exempt — must be a *connect.Error: authn writes it outside the interceptor chain, so an *apperr.Error would not be translated
	attachDetails(ctx, cerr, apperr.ReasonUnauthenticated)
	return cerr
}

// WithSDKAuth authenticates via API key from the x-api-key header
// or api_key query parameter (fallback for beacon requests).
// Accepts both public (pub_) and private (prv_) keys. Public keys are accepted
// via header or query parameter; private keys are header-only.
// Unlike WithDualAuth, there is no JWT fallback — Customer is always nil.
func WithSDKAuth(repo projectKeyLookup) authn.AuthFunc {
	return func(ctx context.Context, req *http.Request) (any, error) {
		apiKey := req.Header.Get(HeaderAPIKey)
		// Fallback to query param for beacon requests, which cannot set headers.
		if apiKey == "" {
			apiKey = req.URL.Query().Get(QueryAPIKey)
			if apiKey != "" && !strings.HasPrefix(apiKey, publicKeyPrefix) {
				return nil, unauthenticated(ctx, "beacon requests only support public API keys")
			}
		}
		if apiKey == "" {
			return nil, unauthenticated(ctx, "x-api-key header not present")
		}

		var project dbread.Project
		var err error
		var authType AuthType
		switch {
		case strings.HasPrefix(apiKey, publicKeyPrefix):
			authType = AuthTypePublicKey
			project, err = repo.GetProjectByPublicApiKey(ctx, apiKey)
		case strings.HasPrefix(apiKey, privateKeyPrefix):
			authType = AuthTypePrivateKey
			project, err = repo.GetProjectByPrivateApiKey(ctx, apiKey)
		default:
			return nil, unauthenticated(ctx, "invalid API key")
		}
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, unauthenticated(ctx, "invalid API key")
			}
			// Repo logs + records non-ErrNoRows DB failures at source.
			return nil, unauthenticated(ctx, "failed to validate API key")
		}

		slog.DebugContext(ctx, "SDK auth succeeded", slog.String("auth_type", string(authType)), slog.String("project_id", project.ID))

		return &Principal{
			AuthType:     authType,
			Project:      &project,
			MaskedAPIKey: maskKey(apiKey),
		}, nil
	}
}

// WithJWTAuth authenticates via JWT in the Authorization header.
// Optionally accepts x-project-id header to populate Project; verifies the
// customer is a member of the project's org via GetProjectByIDAndOrgMember.
func WithJWTAuth(jwtKey []byte, queries *dbread.Queries) authn.AuthFunc {
	return func(ctx context.Context, req *http.Request) (any, error) {
		authHeader := req.Header.Get("Authorization")
		if authHeader == "" {
			return nil, unauthenticated(ctx, "Authorization header not present")
		}

		if !strings.HasPrefix(authHeader, bearerPrefix) {
			return nil, unauthenticated(ctx, "Authorization header must start with Bearer")
		}

		token := strings.TrimPrefix(authHeader, bearerPrefix)
		if token == "" {
			return nil, unauthenticated(ctx, "Bearer token is empty")
		}

		parsedJWT, err := jwt.Parse(token, func(t *jwt.Token) (any, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, authn.Errorf("unexpected signing method: %v", t.Header["alg"])
			}
			return jwtKey, nil
		},
			// Defense in depth: pin the algorithm (the keyfunc already rejects
			// non-HMAC, but WithValidMethods stops a forged header before the
			// keyfunc runs) and require the aud/iss/exp our issuer sets, so a
			// token minted for a different audience or signed without an expiry
			// is rejected rather than silently accepted.
			jwt.WithValidMethods([]string{"HS256"}),
			jwt.WithIssuer(coreauth.Issuer),
			jwt.WithAudience(coreauth.Audience),
			jwt.WithExpirationRequired(),
		)
		if err != nil {
			return nil, unauthenticated(ctx, "invalid authorization")
		}

		if !parsedJWT.Valid {
			return nil, unauthenticated(ctx, "invalid authorization")
		}

		customerID, err := parsedJWT.Claims.GetSubject()
		if err != nil {
			slog.ErrorContext(ctx, "unable to get subject from JWT", slogx.Error(err))
			telemetry.RecordError(ctx, err)
			return nil, unauthenticated(ctx, "invalid token claims")
		}

		var jwtID string
		if claims, ok := parsedJWT.Claims.(jwt.MapClaims); !ok {
			slog.WarnContext(ctx, "JWT claims are not MapClaims, cannot extract jti", slog.String("claims_type", fmt.Sprintf("%T", parsedJWT.Claims)))
		} else if raw, exists := claims["jti"]; exists {
			if s, ok := raw.(string); ok {
				jwtID = s
			} else {
				slog.WarnContext(ctx, "jti claim is not a string", slog.String("jti_type", fmt.Sprintf("%T", raw)))
			}
		}

		customer, err := queries.GetCustomerByID(ctx, customerID)
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				slog.ErrorContext(ctx, "unable to get customer", slogx.Error(err))
				telemetry.RecordError(ctx, err)
			}
			return nil, unauthenticated(ctx, "invalid authorization")
		}

		principal := &Principal{
			AuthType: AuthTypeJWT,
			Customer: &customer,
			JWTID:    jwtID,
		}

		// Optionally populate Project if x-project-id header is provided
		if projectID := req.Header.Get(HeaderProjectID); projectID != "" {
			project, err := queries.GetProjectByIDAndOrgMember(ctx, dbread.GetProjectByIDAndOrgMemberParams{
				ID:         projectID,
				CustomerID: customerID,
			})
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return nil, unauthenticated(ctx, "project not found or access denied")
				}
				slog.ErrorContext(ctx, "unable to get project", slogx.Error(err))
				telemetry.RecordError(ctx, err)
				return nil, unauthenticated(ctx, "failed to verify project access")
			}
			principal.Project = &project
		}

		return principal, nil
	}
}

// WithDualAuth authenticates via private API key if x-api-key header is present; otherwise falls back to JWT.
// Unlike WithSDKAuth, this only accepts private keys (not public) and falls back to JWT, populating Customer.
func WithDualAuth(jwtKey []byte, queries *dbread.Queries, repo projectKeyLookup) authn.AuthFunc {
	jwtAuth := WithJWTAuth(jwtKey, queries)

	return func(ctx context.Context, req *http.Request) (any, error) {
		if apiKey := req.Header.Get(HeaderAPIKey); apiKey != "" {
			if !strings.HasPrefix(apiKey, privateKeyPrefix) {
				return nil, unauthenticated(ctx, "invalid API key")
			}
			project, err := repo.GetProjectByPrivateApiKey(ctx, apiKey)
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return nil, unauthenticated(ctx, "invalid API key")
				}
				// Repo logs + records non-ErrNoRows DB failures at source.
				return nil, unauthenticated(ctx, "failed to validate API key")
			}
			return &Principal{
				AuthType:     AuthTypePrivateKey,
				Project:      &project,
				MaskedAPIKey: maskKey(apiKey),
			}, nil
		}
		return jwtAuth(ctx, req)
	}
}

// getPrincipalFromContext extracts the Principal from context.
func getPrincipalFromContext(ctx context.Context) (*Principal, error) {
	info := authn.GetInfo(ctx)

	if principal, ok := info.(*Principal); ok {
		return principal, nil
	}

	return nil, authn.Errorf("context value is not a Principal type: %T", info)
}

// MustGetPrincipalWithCustomer extracts the Principal and ensures Customer is set.
// Use this for dashboard handlers that require JWT auth context.
func MustGetPrincipalWithCustomer(ctx context.Context) (*Principal, error) {
	principal, err := getPrincipalFromContext(ctx)
	if err != nil {
		slog.DebugContext(ctx, "principal extraction failed", slogx.Error(err))
		return nil, apperr.Unauthenticated(apperr.ReasonUnauthenticated, "unauthenticated")
	}

	if principal.Customer == nil {
		slog.DebugContext(ctx, "customer not set in principal")
		return nil, apperr.Unauthenticated(apperr.ReasonUnauthenticated, "unauthenticated")
	}

	return principal, nil
}

// MustGetPrincipalWithProject extracts the Principal and ensures Project is set.
// Use this for SDK and shared handlers that require a project context.
func MustGetPrincipalWithProject(ctx context.Context) (*Principal, error) {
	principal, err := getPrincipalFromContext(ctx)
	if err != nil {
		slog.DebugContext(ctx, "principal extraction failed", slogx.Error(err))
		return nil, apperr.Unauthenticated(apperr.ReasonUnauthenticated, "unauthenticated")
	}

	if principal.Project == nil {
		slog.DebugContext(ctx, "project not set in principal")
		return nil, apperr.Unauthenticated(apperr.ReasonUnauthenticated, "unauthenticated")
	}

	return principal, nil
}
