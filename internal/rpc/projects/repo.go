package projects

import (
	"context"

	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Repo interface {
	CreateProject(ctx context.Context, arg dbwrite.CreateProjectParams) (dbwrite.Project, error)
	GetProjectById(ctx context.Context, id string) (dbread.Project, error)
	GetProjectsByCustomerId(ctx context.Context, customerID string) ([]dbread.Project, error)
	DeleteProject(ctx context.Context, arg dbwrite.DeleteProjectParams) (dbwrite.Project, error)
	UpdateProjectDisplayName(ctx context.Context, arg dbwrite.UpdateProjectDisplayNameParams) (dbwrite.Project, error)
	UpdateFCMServiceJSON(ctx context.Context, arg dbwrite.UpdateFCMServiceJSONParams) (dbwrite.Project, error)
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

func (r *repoImpl) CreateProject(ctx context.Context, arg dbwrite.CreateProjectParams) (dbwrite.Project, error) {
	return r.write.CreateProject(ctx, arg)
}

func (r *repoImpl) GetProjectById(ctx context.Context, id string) (dbread.Project, error) {
	return r.read.GetProjectById(ctx, id)
}

func (r *repoImpl) GetProjectsByCustomerId(ctx context.Context, customerID string) ([]dbread.Project, error) {
	return r.read.GetProjectsByCustomerId(ctx, customerID)
}

func (r *repoImpl) DeleteProject(ctx context.Context, arg dbwrite.DeleteProjectParams) (dbwrite.Project, error) {
	return r.write.DeleteProject(ctx, arg)
}

func (r *repoImpl) UpdateProjectDisplayName(ctx context.Context, arg dbwrite.UpdateProjectDisplayNameParams) (dbwrite.Project, error) {
	return r.write.UpdateProjectDisplayName(ctx, arg)
}

func (r *repoImpl) UpdateFCMServiceJSON(ctx context.Context, arg dbwrite.UpdateFCMServiceJSONParams) (dbwrite.Project, error) {
	return r.write.UpdateFCMServiceJSON(ctx, arg)
}
