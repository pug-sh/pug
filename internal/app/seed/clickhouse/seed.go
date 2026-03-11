package seed

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Seeder writes mock events directly into ClickHouse for a given project.
type Seeder struct {
	deps *deps
}

func NewSeeder(deps *deps) *Seeder {
	return &Seeder{deps: deps}
}

// Run fetches the first customer and their first project from PostgreSQL,
// then inserts count events into ClickHouse in batches of batchSize.
func (s *Seeder) Run(ctx context.Context, count int64, batchSize int) error {
	projectID, err := s.resolveProjectID(ctx)
	if err != nil {
		return err
	}

	slog.InfoContext(ctx, "seeding events",
		slog.String("project_id", projectID),
		slog.Int64("total", count),
		slog.Int("batch_size", batchSize),
	)

	// Clear existing events
	slog.InfoContext(ctx, "truncating events table")
	if err := s.deps.ch.Exec(ctx, "TRUNCATE TABLE events"); err != nil {
		return fmt.Errorf("truncate failed: %w", err)
	}

	end := time.Now().AddDate(0, 1, 0) // 1 month in the future
	start := end.AddDate(0, -4, 0)     // 4 months before end = 3 months past + 1 month future

	var inserted int64
	startTime := time.Now()

	for inserted < count {
		select {
		case <-ctx.Done():
			slog.InfoContext(ctx, "seed interrupted", slog.Int64("inserted", inserted))
			return nil
		default:
		}

		size := min(int64(batchSize), count-inserted)

		if err := s.insertBatch(ctx, projectID, start, end, int(size)); err != nil {
			return fmt.Errorf("batch insert failed at offset %d: %w", inserted, err)
		}

		inserted += size
		elapsed := time.Since(startTime)
		rate := float64(inserted) / elapsed.Seconds()
		slog.InfoContext(ctx, "progress",
			slog.Int64("inserted", inserted),
			slog.Int64("total", count),
			slog.String("rate", fmt.Sprintf("%.0f events/s", rate)),
			slog.String("elapsed", elapsed.Round(time.Second).String()),
		)
	}

	slog.InfoContext(ctx, "seed complete",
		slog.Int64("inserted", inserted),
		slog.String("elapsed", time.Since(startTime).Round(time.Second).String()),
	)
	return nil
}

func (s *Seeder) insertBatch(ctx context.Context, projectID string, start, end time.Time, size int) error {
	batch, err := s.deps.ch.PrepareBatch(ctx,
		"INSERT INTO events (event_id, project_id, distinct_id, kind, auto_properties, custom_properties, occur_time)")
	if err != nil {
		return err
	}

	for range size {
		e := randomEvent(start, end)
		if err := batch.Append(
			e.eventID,
			projectID,
			e.distinctID,
			e.kind,
			e.autoProperties,
			e.customProperties,
			e.occurTime,
		); err != nil {
			return err
		}
	}

	return batch.Send()
}

func (s *Seeder) resolveProjectID(ctx context.Context) (string, error) {
	var customerID string
	err := s.deps.pg.QueryRow(ctx, "SELECT id FROM customers ORDER BY create_time LIMIT 1").Scan(&customerID)
	if err != nil {
		return "", fmt.Errorf("no customers found: %w", err)
	}

	var projectID string
	err = s.deps.pg.QueryRow(ctx,
		"SELECT id FROM projects WHERE customer_id = $1 ORDER BY create_time LIMIT 1", customerID,
	).Scan(&projectID)
	if err != nil {
		return "", fmt.Errorf("no projects found for customer %s: %w", customerID, err)
	}

	slog.InfoContext(ctx, "resolved target",
		slog.String("customer_id", customerID),
		slog.String("project_id", projectID),
	)
	return projectID, nil
}

// Run is the CLI entry point. It wires dependencies and delegates to Seeder.
func Run(ctx context.Context, count int64, batchSize int) error {
	d, err := newDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	return NewSeeder(d).Run(ctx, count, batchSize)
}
