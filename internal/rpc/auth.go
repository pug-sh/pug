package rpc

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"connectrpc.com/authn"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/pkg/logger/slogx"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
)

const (
	HeaderAPIKey    = "x-api-key"
	HeaderProjectID = "x-project-id"
	bearerPrefix    = "Bearer "
)

// AuthType indicates which authentication method was used
type AuthType int

const (
	AuthTypeUnknown AuthType = iota
	AuthTypeJWT              // Dashboard user authenticated with JWT
	AuthTypeAPIKey           // SDK authenticated with API key
)

// Principal represents the authenticated entity.
// Customer is always set. Project is set for API key auth and JWT auth with x-project-id header.
type Principal struct {
	AuthType AuthType
	Customer dbread.Customer
	Project  *dbread.Project
}

// WithAPIKeyAuth authenticates via API key in the x-api-key header.
func WithAPIKeyAuth(queries *dbread.Queries) authn.AuthFunc {
	return func(ctx context.Context, req *http.Request) (any, error) {
		apiKey := req.Header.Get(HeaderAPIKey)
		if apiKey == "" {
			return nil, authn.Errorf("x-api-key header not present")
		}

		row, err := queries.GetProjectAndCustomerByApiKey(ctx, apiKey)
		if err != nil {
			if err == pgx.ErrNoRows {
				return nil, authn.Errorf("invalid API key")
			}
			slog.ErrorContext(ctx, "error querying project by API key", slogx.Error(err))
			return nil, authn.Errorf("failed to validate API key")
		}

		return &Principal{
			AuthType: AuthTypeAPIKey,
			Customer: row.Customer,
			Project:  &row.Project,
		}, nil
	}
}

// WithJWTAuth authenticates via JWT in the Authorization header.
// Optionally accepts x-project-id header to populate Project.
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
			slog.ErrorContext(ctx, "unable to get customer", slogx.Error(err))
			return nil, authn.Errorf("invalid authorization")
		}

		principal := &Principal{
			AuthType: AuthTypeJWT,
			Customer: customer,
		}

		// Optionally populate Project if x-project-id header is provided
		if projectID := req.Header.Get(HeaderProjectID); projectID != "" {
			project, err := queries.GetProjectByIDAndCustomerID(ctx, dbread.GetProjectByIDAndCustomerIDParams{
				ID:         projectID,
				CustomerID: customerID,
			})
			if err != nil {
				if err == pgx.ErrNoRows {
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

// WithDualAuth tries API key first, then JWT.
func WithDualAuth(jwtKey []byte, queries *dbread.Queries) authn.AuthFunc {
	apiKeyAuth := WithAPIKeyAuth(queries)
	jwtAuth := WithJWTAuth(jwtKey, queries)

	return func(ctx context.Context, req *http.Request) (any, error) {
		if req.Header.Get(HeaderAPIKey) != "" {
			return apiKeyAuth(ctx, req)
		}
		return jwtAuth(ctx, req)
	}
}

// GetPrincipalFromContext extracts the Principal from context.
func GetPrincipalFromContext(ctx context.Context) (*Principal, error) {
	info := authn.GetInfo(ctx)

	if principal, ok := info.(*Principal); ok {
		return principal, nil
	}

	return nil, authn.Errorf("context value is not a Principal type: %T", info)
}

// MustGetPrincipalWithProject extracts the Principal and ensures Project is set.
// Use this for SDK and shared handlers that require a project context.
func MustGetPrincipalWithProject(ctx context.Context) (*Principal, error) {
	principal, err := GetPrincipalFromContext(ctx)
	if err != nil {
		return nil, err
	}

	if principal.Project == nil {
		return nil, authn.Errorf("project not set in principal")
	}

	return principal, nil
}
