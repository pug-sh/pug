package sdk

import (
	"context"
	"net/http"
	"strings"

	"connectrpc.com/authn"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/pkg/constant"
	"github.com/jackc/pgx/v5"
)

// WithAPIKeyAuth provides authentication via API key in the Authorization header
func WithAPIKeyAuth(queries *dbread.Queries) authn.AuthFunc {
	return func(ctx context.Context, req *http.Request) (any, error) {
		authHeader := req.Header.Get(constant.HeaderAuthorization)
		if authHeader == "" {
			return nil, authn.Errorf("authorization header not present")
		}

		if !strings.HasPrefix(authHeader, constant.BearerPrefix) {
			return nil, authn.Errorf("invalid authorization format, expected Bearer prefix")
		}

		apiKey := strings.TrimPrefix(authHeader, constant.BearerPrefix)
		if apiKey == "" {
			return nil, authn.Errorf("API key is empty")
		}

		project, err := queries.GetProjectByApiKey(ctx, apiKey)
		if err != nil {
			// todo - remove before prod
			if err == pgx.ErrNoRows {
				return nil, authn.Errorf("invalid API key")
			}

			return nil, authn.Errorf("error querying project by API key: %v", err)
		}

		return project, nil
	}
}

func GetProjectFromContext(ctx context.Context) (dbread.Project, error) {
	projectCtx := authn.GetInfo(ctx)

	if project, ok := projectCtx.(dbread.Project); ok {
		return project, nil
	}

	return dbread.Project{}, authn.Errorf("context value is not a Project type: %T", projectCtx)
}
