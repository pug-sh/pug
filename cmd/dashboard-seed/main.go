// dashboard-seed re-seeds the trends, funnel, and retention stress dashboards for the given project.
// It drops any existing dashboards for the project (cascade-deletes tiles) and recreates
// all three 8-tile dashboards via Seeder.RunDashboardOnly.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	pgseed "github.com/pug-sh/pug/internal/app/seed/postgres"
)

func main() {
	pgDSN := flag.String("pg", "postgres://postgres:postgres@localhost:5433/pug?sslmode=disable", "Postgres DSN")
	projectID := flag.String("project", "", "project ID to seed the dashboard under")
	flag.Parse()

	if *projectID == "" {
		fmt.Fprintln(os.Stderr, "ERROR: -project is required")
		os.Exit(2)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, *pgDSN)
	if err != nil {
		die("connect postgres: %v", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		die("ping postgres: %v", err)
	}

	if err := pgseed.NewSeederFromPool(pool).RunDashboardOnly(ctx, *projectID); err != nil {
		die("seed dashboard: %v", err)
	}

	fmt.Printf("dashboard seeded for project %s\n", *projectID)
}

func die(format string, args ...any) {
	fmt.Fprintln(os.Stderr, "ERROR: "+fmt.Sprintf(format, args...))
	os.Exit(1)
}
