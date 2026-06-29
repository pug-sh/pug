package seed

import (
	"github.com/jackc/pgx/v5/pgxpool"
)

type deps struct {
	pg *pgxpool.Pool
}
