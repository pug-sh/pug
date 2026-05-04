package profiles

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
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
