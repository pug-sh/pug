package profiles

import (
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Worker struct {
	PgW   *pgxpool.Pool
	Read  *dbread.Queries
	Write *dbwrite.Queries
}

func NewWorker(pgRO, pgW *pgxpool.Pool) *Worker {
	return &Worker{
		PgW:   pgW,
		Read:  dbread.New(pgRO),
		Write: dbwrite.New(pgW),
	}
}
