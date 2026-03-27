package seed

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/google/uuid"
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
// If file is provided, it imports from that CSV instead of generating random data.
func (s *Seeder) Run(ctx context.Context, count int64, batchSize int, file string) error {
	projectID, err := s.resolveProjectID(ctx)
	if err != nil {
		return err
	}

	if file != "" {
		return s.runFromCSV(ctx, projectID, file, batchSize)
	}

	slog.InfoContext(ctx, "seeding events",
		slog.String("project_id", projectID),
		slog.Int64("total", count),
		slog.Int("batch_size", batchSize),
	)

	slog.InfoContext(ctx, "truncating events table")
	if err := s.deps.ch.Exec(ctx, "TRUNCATE TABLE events"); err != nil {
		return fmt.Errorf("truncate failed: %w", err)
	}

	end := time.Now().AddDate(0, 1, 0)
	start := end.AddDate(0, -4, 0)

	slog.InfoContext(ctx, "building session pool")
	sessionPool := buildSessionPool(start, end)
	slog.InfoContext(ctx, "session pool ready", slog.Int("pool_size", len(sessionPool)))

	var inserted int64
	startTime := time.Now()

	for inserted < count {
		select {
		case <-ctx.Done():
			slog.InfoContext(ctx, "seed interrupted", slog.Int64("inserted", inserted))
			return ctx.Err()
		default:
		}

		size := min(int64(batchSize), count-inserted)

		n, err := s.insertBatch(ctx, projectID, sessionPool, int(size))
		if err != nil {
			return fmt.Errorf("batch insert failed at offset %d: %w", inserted, err)
		}

		inserted += int64(n)
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

func (s *Seeder) insertBatch(ctx context.Context, projectID string, pool [][]event, size int) (int, error) {
	batch, err := s.deps.ch.PrepareBatch(ctx,
		"INSERT INTO events (event_id, project_id, distinct_id, kind, auto_properties, custom_properties, occur_time, session_id)")
	if err != nil {
		return 0, err
	}

	inserted := 0
	for inserted < size {
		sess := randomSessionFromPool(pool)
		for _, e := range sess {
			if inserted >= size {
				break
			}
			if err := batch.Append(
				e.eventID,
				projectID,
				e.distinctID,
				e.kind,
				e.autoProperties,
				e.customProperties,
				e.occurTime,
				e.sessionID,
			); err != nil {
				return 0, err
			}
			inserted++
		}
	}

	return inserted, batch.Send()
}

func (s *Seeder) resolveProjectID(ctx context.Context) (string, error) {
	var projectID string
	err := s.deps.pg.QueryRow(ctx,
		"SELECT p.id FROM projects p JOIN org_members om ON om.org_id = p.org_id JOIN customers c ON c.id = om.customer_id ORDER BY p.create_time LIMIT 1",
	).Scan(&projectID)
	if err != nil {
		return "", fmt.Errorf("no projects found: %w", err)
	}

	slog.InfoContext(ctx, "resolved target",
		slog.String("project_id", projectID),
	)
	return projectID, nil
}

func (s *Seeder) runFromCSV(ctx context.Context, projectID, file string, batchSize int) error {
	slog.InfoContext(ctx, "importing from CSV",
		slog.String("project_id", projectID),
		slog.String("file", file),
		slog.Int("batch_size", batchSize),
	)

	slog.InfoContext(ctx, "truncating events table")
	if err := s.deps.ch.Exec(ctx, "TRUNCATE TABLE events"); err != nil {
		return fmt.Errorf("truncate failed: %w", err)
	}

	reader, err := newRees46Reader(file)
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close() }()

	var inserted int64
	startTime := time.Now()

	for {
		select {
		case <-ctx.Done():
			slog.InfoContext(ctx, "import interrupted", slog.Int64("inserted", inserted))
			return ctx.Err()
		default:
		}

		batch, err := s.deps.ch.PrepareBatch(ctx,
			"INSERT INTO events (event_id, project_id, distinct_id, kind, auto_properties, custom_properties, occur_time, session_id)")
		if err != nil {
			return err
		}

		for i := 0; i < batchSize; i++ {
			e, err := reader.Read()
			if err != nil {
				if errors.Is(err, io.EOF) {
					slog.InfoContext(ctx, "import complete",
						slog.Int64("inserted", inserted),
						slog.String("elapsed", time.Since(startTime).Round(time.Second).String()),
					)
					return batch.Send()
				}
				return fmt.Errorf("read record: %w", err)
			}

			if err := batch.Append(
				e.eventID,
				projectID,
				e.distinctID,
				e.kind,
				e.autoProperties,
				e.customProperties,
				e.occurTime,
				uuid.NewString(),
			); err != nil {
				return err
			}
			inserted++
		}

		if err := batch.Send(); err != nil {
			return fmt.Errorf("batch send failed at offset %d: %w", inserted, err)
		}

		elapsed := time.Since(startTime)
		rate := float64(inserted) / elapsed.Seconds()
		slog.InfoContext(ctx, "progress",
			slog.Int64("inserted", inserted),
			slog.String("rate", fmt.Sprintf("%.0f events/s", rate)),
			slog.String("elapsed", elapsed.Round(time.Second).String()),
		)
	}
}

// Run is the CLI entry point. It wires dependencies and delegates to Seeder.
func Run(ctx context.Context, count int64, batchSize int, file string) error {
	d, err := newDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close(ctx)

	return NewSeeder(d).Run(ctx, count, batchSize, file)
}
