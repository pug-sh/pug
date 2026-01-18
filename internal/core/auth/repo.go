package auth

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

func newRepo(pgRO *pgxpool.Pool, pgW *pgxpool.Pool) *repo {
	return &repo{
		read:  dbread.New(pgRO),
		write: dbwrite.New(pgW),
	}
}

func (r *repo) GetCustomerByEmail(ctx context.Context, email string) (dbread.Customer, error) {
	return r.read.GetCustomerByEmail(ctx, email)
}

func (r *repo) GetCustomerByEmailWithPassword(ctx context.Context, email string) (dbread.GetCustomerByEmailWithPasswordRow, error) {
	return r.read.GetCustomerByEmailWithPassword(ctx, email)
}

func (r *repo) GetCustomerByID(ctx context.Context, id string) (dbread.Customer, error) {
	return r.read.GetCustomerByID(ctx, id)
}

func (r *repo) CreateCustomer(ctx context.Context, arg dbwrite.CreateCustomerParams) (dbwrite.Customer, error) {
	return r.write.CreateCustomer(ctx, arg)
}

func (r *repo) CreateProject(ctx context.Context, arg dbwrite.CreateProjectParams) (dbwrite.Project, error) {
	return r.write.CreateProject(ctx, arg)
}
