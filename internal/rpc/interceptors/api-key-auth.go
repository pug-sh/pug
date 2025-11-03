package interceptors

import (
	"context"
	"net/http"
	"strings"

	"connectrpc.com/authn"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/jackc/pgx/v5"
)

// Header constants to avoid typos and make code more readable
const (
	HeaderAuthorization = "Authorization"
	BearerPrefix        = "Bearer "
)

// APIKeyAuth provides authentication via API key in the Authorization header
func APIKeyAuth(queries *dbread.Queries) authn.AuthFunc {
	return func(ctx context.Context, req *http.Request) (any, error) {
		// Get the Authorization header
		authHeader := req.Header.Get(HeaderAuthorization)

		// Check if the Authorization header is present
		if authHeader == "" {
			return nil, authn.Errorf("authorization header not present")
		}

		if !strings.HasPrefix(authHeader, BearerPrefix) {
			return nil, authn.Errorf("invalid authorization format, expected Bearer prefix")
		}

		var apiKey string
		apiKey = strings.TrimPrefix(authHeader, BearerPrefix)

		if apiKey == "" {
			return nil, authn.Errorf("API key is empty")
		}

		project, err := queries.GetProjectByApiKey(ctx, apiKey)

		if err != nil {
			if err == pgx.ErrNoRows {
				return nil, authn.Errorf("invalid API key")
			}

			return nil, authn.Errorf("error querying project by API key: %v", err)
		}

		// Store project as value in context
		return project, nil
	}
}

// GetProjectFromContext retrieves the project from the context that was stored by the APIKeyAuth middleware
func GetProjectFromContext(ctx context.Context) (dbread.Project, error) {
	// The authn middleware stores the value under authn.UserContextKey()
	projectCtx := authn.GetInfo(ctx)

	if project, ok := projectCtx.(dbread.Project); ok {
		return project, nil
	}

	return dbread.Project{}, authn.Errorf("context value is not a Project type: %T", projectCtx)
}
