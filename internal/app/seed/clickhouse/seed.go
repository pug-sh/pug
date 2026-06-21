package seed

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/chcol"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pug-sh/pug/internal/autoprop"
	chq "github.com/pug-sh/pug/internal/core/clickhouse"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
)

// profilesInsertStmt is the column list for copying profiles into ClickHouse.
// Shared between the initial batch and each re-prepared flush so the two can't
// drift.
const profilesInsertStmt = "INSERT INTO profiles (id, project_id, external_id, properties, is_deleted, create_time, update_time)"

type Seeder struct {
	deps *deps
}

func NewSeeder(deps *deps) *Seeder {
	return &Seeder{deps: deps}
}

// autoAnyMapToVariantMap wraps a map[string]any into the chcol.Variant map
// shape the clickhouse-go driver expects for Map(String, Variant(...)) columns.
// Type-routing is by Go static type. String values for $-prefixed auto-property
// keys are routed through autoprop.Variant for typed inference; custom keys
// fall through to the String slot.
func autoAnyMapToVariantMap(ctx context.Context, projectID string, props map[string]any) map[string]chcol.Variant {
	if len(props) == 0 {
		return nil
	}
	out := make(map[string]chcol.Variant, len(props))
	for k, v := range props {
		switch x := v.(type) {
		case string:
			out[k] = autoprop.Variant(ctx, projectID, k, x)
		case bool:
			out[k] = chcol.NewVariantWithType(x, "Bool")
		case int:
			out[k] = chcol.NewVariantWithType(int64(x), "Int64")
		case int64:
			out[k] = chcol.NewVariantWithType(x, "Int64")
		case float64:
			out[k] = chcol.NewVariantWithType(x, "Float64")
		default:
			out[k] = chcol.NewVariantWithType(fmt.Sprint(x), "String")
		}
	}
	return out
}

func (s *Seeder) Run(ctx context.Context, count int64, batchSize int, truncate bool) error {
	projectID, err := s.resolveProjectID(ctx)
	if err != nil {
		return err
	}

	if err := s.runProfiles(ctx, projectID, truncate); err != nil {
		return fmt.Errorf("seed profiles: %w", err)
	}

	if truncate {
		slog.InfoContext(ctx, "truncating events table")
		if err := s.deps.ch.Exec(ctx, "TRUNCATE TABLE events"); err != nil {
			return fmt.Errorf("truncate failed: %w", err)
		}
	} else {
		slog.InfoContext(ctx, "skipping truncation, appending to existing data")
	}

	return s.backfillEvents(ctx, projectID, count, batchSize)
}

// backfillEvents generates `count` synthetic events for projectID and appends
// them to the events table. It does not truncate; callers decide reset policy.
func (s *Seeder) backfillEvents(ctx context.Context, projectID string, count int64, batchSize int) error {
	slog.InfoContext(ctx, "seeding events",
		slog.String("project_id", projectID),
		slog.Int64("total", count),
		slog.Int("batch_size", batchSize),
	)

	seedStart := time.Now()
	start := seedStart.AddDate(0, -4, 0)

	slog.InfoContext(ctx, "building session factory")
	factory := newSessionFactory()
	slog.InfoContext(ctx, "session factory ready", slog.Int("users", len(factory.users)))

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
		// Refresh each batch so re-anchoring and the insert clamp use live time, not
		// a stale seed-start instant (long runs can otherwise leave a recent dead zone).
		end := time.Now()

		n, err := s.insertBatch(ctx, projectID, factory, int(size), start, end)
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

// EventCount returns how many events are stored for projectID. The demo worker
// uses it to decide whether a one-time backfill is needed before live traffic.
func EventCount(ctx context.Context, ch driver.Conn, projectID string) (uint64, error) {
	var n uint64
	if err := ch.QueryRow(ctx, "SELECT count() FROM events WHERE project_id = ?", projectID).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// Backfill copies profiles from Postgres to ClickHouse and appends `count`
// synthetic historical events for projectID, without truncating. It is the
// programmatic entry point used by the demo worker to populate an empty demo
// project; the CLI path goes through Run (which also handles truncation).
func Backfill(ctx context.Context, pg *pgxpool.Pool, ch driver.Conn, projectID string, count int64, batchSize int) error {
	s := &Seeder{deps: &deps{pg: pg, ch: ch}}
	if err := s.runProfiles(ctx, projectID, false); err != nil {
		return fmt.Errorf("seed profiles: %w", err)
	}
	return s.backfillEvents(ctx, projectID, count, batchSize)
}

func (s *Seeder) insertBatch(ctx context.Context, projectID string, factory *sessionFactory, size int, start, end time.Time) (int, error) {
	batch, err := s.deps.ch.PrepareBatch(ctx, chq.EventsInsertStmt)
	if err != nil {
		return 0, err
	}

	inserted := 0
	// Sessions are atomic and variable-length, so keep pulling whole sessions
	// until the batch is full, truncating the final session at `size`.
	for inserted < size {
		sess := factory.session(start, end)
		if len(sess) == 0 {
			// Defensive: every journey has ≥1 step and botSession emits ≥2, so a
			// session is never empty today. Guard anyway so a future zero-step
			// journey can't spin this loop forever — the ctx cancellation check
			// lives one level up in backfillEvents.
			break
		}
		for _, e := range sess {
			if inserted >= size {
				break
			}
			promoted, restAuto := chq.SplitPromotedAutoAnyProperties(e.autoProperties)
			args := []any{
				e.eventID,
				projectID,
				e.distinctID,
				e.kind,
				autoAnyMapToVariantMap(ctx, projectID, restAuto),
				autoAnyMapToVariantMap(ctx, projectID, e.customProperties),
			}
			args = append(args, promoted.AppendArgs()...)
			occurTime := clampOccurTime(e.occurTime, end)
			args = append(args, occurTime, e.sessionID)
			if err := batch.Append(args...); err != nil {
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

	batch, err := s.deps.ch.PrepareBatch(ctx, profilesInsertStmt)
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
			batch, err = s.deps.ch.PrepareBatch(ctx, profilesInsertStmt)
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

func Run(ctx context.Context, count int64, batchSize int, truncate bool) error {
	d, err := newDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close(ctx)

	return NewSeeder(d).Run(ctx, count, batchSize, truncate)
}
