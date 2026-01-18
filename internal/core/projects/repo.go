package projects

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

func (r *repo) CreateProject(ctx context.Context, arg dbwrite.CreateProjectParams) (dbwrite.Project, error) {
	return r.write.CreateProject(ctx, arg)
}

func (r *repo) GetProjectById(ctx context.Context, id string) (dbread.Project, error) {
	return r.read.GetProjectById(ctx, id)
}

func (r *repo) GetProjectsByCustomerId(ctx context.Context, customerID string) ([]dbread.Project, error) {
	return r.read.GetProjectsByCustomerId(ctx, customerID)
}

func (r *repo) ProjectExistsForCustomer(ctx context.Context, arg dbread.ProjectExistsForCustomerParams) (bool, error) {
	return r.read.ProjectExistsForCustomer(ctx, arg)
}

func (r *repo) DeleteProject(ctx context.Context, arg dbwrite.DeleteProjectParams) (dbwrite.Project, error) {
	return r.write.DeleteProject(ctx, arg)
}

func (r *repo) UpdateProjectDisplayName(ctx context.Context, arg dbwrite.UpdateProjectDisplayNameParams) (dbwrite.Project, error) {
	return r.write.UpdateProjectDisplayName(ctx, arg)
}

func (r *repo) UpdateFCMServiceJSON(ctx context.Context, arg dbwrite.UpdateFCMServiceJSONParams) (dbwrite.Project, error) {
	return r.write.UpdateFCMServiceJSON(ctx, arg)
}
