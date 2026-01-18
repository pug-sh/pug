package users

import (
	"context"

	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
)

type Repo interface {
	GetCustomerByEmail(ctx context.Context, email string) (dbread.Customer, error)
	GetCustomerByEmailWithPassword(ctx context.Context, email string) (dbread.GetCustomerByEmailWithPasswordRow, error)
	GetCustomerByID(ctx context.Context, id string) (dbread.Customer, error)
	CreateCustomer(ctx context.Context, arg dbwrite.CreateCustomerParams) (dbwrite.Customer, error)
	CreateProject(ctx context.Context, arg dbwrite.CreateProjectParams) (dbwrite.Project, error)
}