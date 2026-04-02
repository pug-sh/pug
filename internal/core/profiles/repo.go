package profiles

import (
	"context"

	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
)

type Repo struct {
	queries *dbread.Queries
}

func NewRepo(queries *dbread.Queries) *Repo {
	if queries == nil {
		panic("profiles: queries is nil")
	}
	return &Repo{queries: queries}
}

func (r *Repo) GetPropertyKeys(ctx context.Context, projectID string) ([]string, error) {
	keys, err := r.queries.GetProfilePropertyKeys(ctx, projectID)
	if err != nil {
		return nil, err
	}
	result := make([]string, len(keys))
	for i, k := range keys {
		result[i] = k.String
	}
	return result, nil
}

func (r *Repo) GetPropertyValues(ctx context.Context, projectID, propertyKey string) ([]string, error) {
	rows, err := r.queries.GetProfilePropertyValues(ctx, dbread.GetProfilePropertyValuesParams{
		ProjectID:   projectID,
		PropertyKey: propertyKey,
	})
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, len(rows))
	for _, v := range rows {
		if s, ok := v.(string); ok {
			result = append(result, s)
		}
	}
	return result, nil
}
