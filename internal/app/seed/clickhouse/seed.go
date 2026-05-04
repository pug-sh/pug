package seed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/google/uuid"
)

type Seeder struct {
	deps *deps
}

func NewSeeder(deps *deps) *Seeder {
	return &Seeder{deps: deps}
}

func (s *Seeder) Run(ctx context.Context, count int64, batchSize int, file string, truncate bool) error {
	projectID, err := s.resolveProjectID(ctx)
	if err != nil {
		return err
	}

	if err := s.runProfiles(ctx, projectID, truncate); err != nil {
		return fmt.Errorf("seed profiles: %w", err)
	}

	if file != "" {
		return s.runFromCSV(ctx, projectID, file, batchSize, truncate)
	}

	slog.InfoContext(ctx, "seeding events",
		slog.String("project_id", projectID),
		slog.Int64("total", count),
		slog.Int("batch_size", batchSize),
	)

	if truncate {
		slog.InfoContext(ctx, "truncating events table")
		if err := s.deps.ch.Exec(ctx, "TRUNCATE TABLE events"); err != nil {
			return fmt.Errorf("truncate failed: %w", err)
		}
	} else {
		slog.InfoContext(ctx, "skipping truncation, appending to existing data")
	}

	end := time.Now().AddDate(0, 1, 0)
	start := end.AddDate(0, -4, 0)

	slog.InfoContext(ctx, "building session pool")
	sessionPool := buildSessionPool(start, end)
	slog.InfoContext(ctx, "session pool ready", slog.Int("pool_size", len(sessionPool)))

	tracker := newUserSessionTracker()
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

		n, err := s.insertBatch(ctx, projectID, sessionPool, int(size), start, end, tracker)
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

func (s *Seeder) insertBatch(ctx context.Context, projectID string, pool [][]event, size int, start, end time.Time, tracker *userSessionTracker) (int, error) {
	batch, err := s.deps.ch.PrepareBatch(ctx,
		"INSERT INTO events (event_id, project_id, distinct_id, kind, auto_properties, custom_properties, occur_time, session_id)")
	if err != nil {
		return 0, err
	}

	inserted := 0
	for inserted < size {
		for _, e := range randomSessionFromPool(pool, start, end, tracker) {
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

func (s *Seeder) runProfiles(ctx context.Context, projectID string, truncate bool) error {
	if truncate {
		slog.InfoContext(ctx, "truncating profiles table")
		if err := s.deps.ch.Exec(ctx, "TRUNCATE TABLE profiles"); err != nil {
			return fmt.Errorf("truncate profiles failed: %w", err)
		}
	}

	slog.InfoContext(ctx, "copying profiles from PostgreSQL to ClickHouse",
		slog.String("project_id", projectID),
	)

	pgRead := dbread.New(s.deps.pg)
	profiles, err := pgRead.GetAllProfilesByProjectID(ctx, projectID)
	if err != nil {
		return fmt.Errorf("query profiles: %w", err)
	}

	batch, err := s.deps.ch.PrepareBatch(ctx,
		"INSERT INTO profiles (id, project_id, external_id, properties, is_deleted, create_time, update_time)")
	if err != nil {
		return fmt.Errorf("prepare profiles batch: %w", err)
	}

	inserted := 0
	for _, p := range profiles {
		propsJSON, err := json.Marshal(p.Properties)
		if err != nil {
			return fmt.Errorf("marshal properties: %w", err)
		}

		// Postgres profile IDs may carry trailing spaces when read via pgx into a plain string;
		// strip them before inserting into ClickHouse to avoid mismatched lookups.
		profileID := strings.TrimRight(p.ID, " ")
		if err := batch.Append(profileID, projectID, p.ExternalID.String, string(propsJSON), uint8(0), p.CreateTime.Time, p.UpdateTime.Time); err != nil {
			return fmt.Errorf("append profile: %w", err)
		}
		inserted++

		if inserted%1000 == 0 {
			if err := batch.Send(); err != nil {
				return fmt.Errorf("send profiles batch: %w", err)
			}
			slog.InfoContext(ctx, "profiles copied",
				slog.Int("inserted", inserted),
			)
			batch, err = s.deps.ch.PrepareBatch(ctx,
				"INSERT INTO profiles (id, project_id, external_id, properties, is_deleted, create_time, update_time)")
			if err != nil {
				return fmt.Errorf("prepare profiles batch: %w", err)
			}
		}
	}

	if err := batch.Send(); err != nil {
		return fmt.Errorf("send profiles batch: %w", err)
	}

	slog.InfoContext(ctx, "profiles copied",
		slog.Int("count", inserted),
	)
	return nil
}

func (s *Seeder) runFromCSV(ctx context.Context, projectID, file string, batchSize int, truncate bool) error {
	slog.InfoContext(ctx, "importing from CSV",
		slog.String("project_id", projectID),
		slog.String("file", file),
		slog.Int("batch_size", batchSize),
	)

	if truncate {
		slog.InfoContext(ctx, "truncating events table")
		if err := s.deps.ch.Exec(ctx, "TRUNCATE TABLE events"); err != nil {
			return fmt.Errorf("truncate failed: %w", err)
		}
	} else {
		slog.InfoContext(ctx, "skipping truncation, appending to existing data")
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

func Run(ctx context.Context, count int64, batchSize int, file string, truncate bool) error {
	d, err := newDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close(ctx)

	return NewSeeder(d).Run(ctx, count, batchSize, file, truncate)
}
