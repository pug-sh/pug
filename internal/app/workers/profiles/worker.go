package profiles

import (
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Worker struct {
	PgW   *pgxpool.Pool
	Write *dbwrite.Queries
}

func NewWorker(pgW *pgxpool.Pool) *Worker {
	return &Worker{
		PgW:   pgW,
		Write: dbwrite.New(pgW),
	}
}
