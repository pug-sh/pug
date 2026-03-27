package rpc

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"connectrpc.com/authn"
	"github.com/fivebitsio/cotton/internal/core/projects"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/slogx"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
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
type AuthType int

const (
	AuthTypeUnknown AuthType = iota
	AuthTypeJWT              // Dashboard user authenticated with JWT
	AuthTypeAPIKey           // SDK authenticated with API key
)

// Principal represents the authenticated entity.
// Customer is set for JWT auth. Project is set for API key auth and JWT auth with x-project-id header.
type Principal struct {
	AuthType AuthType
	Customer *dbread.Customer
	Project  *dbread.Project
}

// WithSDKAuth authenticates via API key from the x-api-key header
// or api_key query parameter (fallback for beacon requests).
// Accepts both public keys (pub_) and private keys (prv_).
func WithSDKAuth(repo *projects.Repo) authn.AuthFunc {
	return func(ctx context.Context, req *http.Request) (any, error) {
		apiKey := req.Header.Get(HeaderAPIKey)
		// Fallback to query param for beacon requests, which cannot set headers.
		if apiKey == "" {
			apiKey = req.URL.Query().Get(QueryAPIKey)
		}
		if apiKey == "" {
			return nil, authn.Errorf("x-api-key header not present")
		}

		var project dbread.Project
		var err error
		switch {
		case strings.HasPrefix(apiKey, publicKeyPrefix):
			project, err = repo.GetProjectByPublicApiKey(ctx, apiKey)
		case strings.HasPrefix(apiKey, privateKeyPrefix):
			project, err = repo.GetProjectByPrivateApiKey(ctx, apiKey)
		default:
			return nil, authn.Errorf("invalid API key")
		}
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, authn.Errorf("invalid API key")
			}
			slog.ErrorContext(ctx, "error querying project by API key", slogx.Error(err))
			return nil, authn.Errorf("failed to validate API key")
		}

		return &Principal{
			AuthType: AuthTypeAPIKey,
			Project:  &project,
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
			return nil, authn.Errorf("Authorization header not present")
		}

		if !strings.HasPrefix(authHeader, bearerPrefix) {
			return nil, authn.Errorf("Authorization header must start with Bearer")
		}

		token := strings.TrimPrefix(authHeader, bearerPrefix)
		if token == "" {
			return nil, authn.Errorf("Bearer token is empty")
		}

		parsedJWT, err := jwt.Parse(token, func(t *jwt.Token) (any, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, authn.Errorf("unexpected signing method: %v", t.Header["alg"])
			}
			return jwtKey, nil
		})
		if err != nil {
			return nil, authn.Errorf("invalid authorization")
		}

		if !parsedJWT.Valid {
			return nil, authn.Errorf("invalid authorization")
		}

		customerID, err := parsedJWT.Claims.GetSubject()
		if err != nil {
			slog.ErrorContext(ctx, "unable to get subject from JWT", slogx.Error(err))
			return nil, authn.Errorf("invalid token claims")
		}

		customer, err := queries.GetCustomerByID(ctx, customerID)
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				slog.ErrorContext(ctx, "unable to get customer", slogx.Error(err))
			}
			return nil, authn.Errorf("invalid authorization")
		}

		principal := &Principal{
			AuthType: AuthTypeJWT,
			Customer: &customer,
		}

		// Optionally populate Project if x-project-id header is provided
		if projectID := req.Header.Get(HeaderProjectID); projectID != "" {
			project, err := queries.GetProjectByIDAndOrgMember(ctx, dbread.GetProjectByIDAndOrgMemberParams{
				ID:         projectID,
				CustomerID: customerID,
			})
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return nil, authn.Errorf("project not found or access denied")
				}
				slog.ErrorContext(ctx, "unable to get project", slogx.Error(err))
				return nil, authn.Errorf("failed to verify project access")
			}
			principal.Project = &project
		}

		return principal, nil
	}
}

// WithDualAuth authenticates via private API key if x-api-key header is present; otherwise falls back to JWT.
func WithDualAuth(jwtKey []byte, queries *dbread.Queries, repo *projects.Repo) authn.AuthFunc {
	jwtAuth := WithJWTAuth(jwtKey, queries)

	return func(ctx context.Context, req *http.Request) (any, error) {
		if apiKey := req.Header.Get(HeaderAPIKey); apiKey != "" {
			if !strings.HasPrefix(apiKey, privateKeyPrefix) {
				return nil, authn.Errorf("invalid API key")
			}
			project, err := repo.GetProjectByPrivateApiKey(ctx, apiKey)
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return nil, authn.Errorf("invalid API key")
				}
				slog.ErrorContext(ctx, "error querying project by private API key", slogx.Error(err))
				return nil, authn.Errorf("failed to validate API key")
			}
			return &Principal{
				AuthType: AuthTypeAPIKey,
				Project:  &project,
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
		return nil, err
	}

	if principal.Customer == nil {
		slog.DebugContext(ctx, "customer not set in principal")
		return nil, authn.Errorf("customer not set in principal")
	}

	return principal, nil
}

// MustGetPrincipalWithProject extracts the Principal and ensures Project is set.
// Use this for SDK and shared handlers that require a project context.
func MustGetPrincipalWithProject(ctx context.Context) (*Principal, error) {
	principal, err := getPrincipalFromContext(ctx)
	if err != nil {
		slog.DebugContext(ctx, "principal extraction failed", slogx.Error(err))
		return nil, err
	}

	if principal.Project == nil {
		slog.DebugContext(ctx, "project not set in principal")
		return nil, authn.Errorf("project not set in principal")
	}

	return principal, nil
}
