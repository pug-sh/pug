package dashboards

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
)

type Service struct {
	read  *dbread.Queries
	write *dbwrite.Queries
	pgW   *pgxpool.Pool // retained for transactional Upsert; reads/writes outside Upsert use the typed handles
}

func NewService(pgRO *pgxpool.Pool, pgW *pgxpool.Pool) *Service {
	return &Service{
		read:  dbread.New(pgRO),
		write: dbwrite.New(pgW),
		pgW:   pgW,
	}
}
