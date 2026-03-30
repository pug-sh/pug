package profiles

import (
	"context"

	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
)

type Repo struct {
	queries *dbread.Queries
}

func NewRepo(queries *dbread.Queries) *Repo {
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
