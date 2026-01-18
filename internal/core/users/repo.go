package users

import (
	"context"

	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/jackc/pgx/v5/pgxpool"
)

type repo struct {
	read  *dbread.Queries
	write *dbwrite.Queries
}

func newRepo(pgRO, pgW *pgxpool.Pool) *repo {
	return &repo{
		read:  dbread.New(pgRO),
		write: dbwrite.New(pgW),
	}
}
func (r *repo) GetUserByID(ctx context.Context, id string) (dbread.User, error) {
	return r.read.GetUserByID(ctx, id)
}

func (r *repo) GetUserByProjectAndExternalID(ctx context.Context, arg dbread.GetUserByProjectAndExternalIDParams) (dbread.User, error) {
	return r.read.GetUserByProjectAndExternalID(ctx, arg)
}

func (r *repo) GetUsersByProjectID(ctx context.Context, id string) ([]dbread.User, error) {
	return r.read.GetUsersByProjectID(ctx, id)
}

func (r *repo) CreateUser(ctx context.Context, arg dbwrite.CreateUserParams) (dbwrite.User, error) {
	return r.write.CreateUser(ctx, arg)
}

func (r *repo) UpdateUserProperties(ctx context.Context, arg dbwrite.UpdateUserPropertiesParams) (dbwrite.User, error) {
	return r.write.UpdateUserProperties(ctx, arg)
}

func (r *repo) UpdateUserCustomProperties(ctx context.Context, arg dbwrite.UpdateUserCustomPropertiesParams) (dbwrite.User, error) {
	return r.write.UpdateUserCustomProperties(ctx, arg)
}

func (r *repo) DeleteUserByID(ctx context.Context, id string) error {
	return r.write.DeleteUserByID(ctx, id)
}
