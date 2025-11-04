package auth

import (
	"context"

	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Repo interface {
	GetCustomerByEmail(ctx context.Context, email string) (dbread.Customer, error)
	GetCustomerByEmailWithPassword(ctx context.Context, email string) (dbread.GetCustomerByEmailWithPasswordRow, error)
	GetCustomerByID(ctx context.Context, id string) (dbread.Customer, error)
	CreateCustomer(ctx context.Context, arg dbwrite.CreateCustomerParams) (dbwrite.Customer, error)
	CreateProject(ctx context.Context, arg dbwrite.CreateProjectParams) (dbwrite.Project, error)
}

type repoImpl struct {
	read  *dbread.Queries
	write *dbwrite.Queries
}

func NewRepo(pgRO *pgxpool.Pool, pgW *pgxpool.Pool) Repo {
	return &repoImpl{
		read:  dbread.New(pgRO),
		write: dbwrite.New(pgW),
	}
}

func (r *repoImpl) GetCustomerByEmail(ctx context.Context, email string) (dbread.Customer, error) {
	return r.read.GetCustomerByEmail(ctx, email)
}

func (r *repoImpl) GetCustomerByEmailWithPassword(ctx context.Context, email string) (dbread.GetCustomerByEmailWithPasswordRow, error) {
	return r.read.GetCustomerByEmailWithPassword(ctx, email)
}

func (r *repoImpl) GetCustomerByID(ctx context.Context, id string) (dbread.Customer, error) {
	return r.read.GetCustomerByID(ctx, id)
}

func (r *repoImpl) CreateCustomer(ctx context.Context, arg dbwrite.CreateCustomerParams) (dbwrite.Customer, error) {
	return r.write.CreateCustomer(ctx, arg)
}

func (r *repoImpl) CreateProject(ctx context.Context, arg dbwrite.CreateProjectParams) (dbwrite.Project, error) {
	return r.write.CreateProject(ctx, arg)
}
