package profiles

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
)

type DB interface {
	dbwrite.DBTX
	Begin(context.Context) (pgx.Tx, error)
}

type Worker struct {
	PgW   DB
	Write *dbwrite.Queries
}

func NewWorker(pgW DB) *Worker {
	return &Worker{
		PgW:   pgW,
		Write: dbwrite.New(pgW),
	}
}
