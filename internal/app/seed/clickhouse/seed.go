package seed

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/chcol"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pug-sh/pug/internal/autoprop"
	chq "github.com/pug-sh/pug/internal/core/clickhouse"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
)

type Seeder struct {
	deps *deps
}

func NewSeeder(deps *deps) *Seeder {
	return &Seeder{deps: deps}
}

// nonFiniteWarned rate-limits the non-finite-float warning to one log per
// property key so a future generator formula that starts emitting NaN/Inf
// surfaces without flooding the insert path.
var nonFiniteWarned sync.Map

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
			// ClickHouse Float64 carries nan/inf natively, so keep the Float64
			// slot — coercing to String would flip the column type per-value. No
			// current generator formula produces a non-finite float; warn once
			// per key so a future one that does surfaces instead of silently
			// storing nan.
			if math.IsNaN(x) || math.IsInf(x, 0) {
				if _, seen := nonFiniteWarned.LoadOrStore(k, struct{}{}); !seen {
					slog.WarnContext(ctx, "non-finite demo property",
						slog.String("key", k), slog.String("value", fmt.Sprint(x)))
				}
			}
			out[k] = chcol.NewVariantWithType(x, "Float64")
		default:
			out[k] = chcol.NewVariantWithType(fmt.Sprint(x), "String")
		}
	}
	return out
}

// backfillEvents generates `count` synthetic events for projectID and appends
// them to the events table. It does not truncate; callers decide reset policy.
// It returns the set of human user indices that actually produced at least one
// inserted event, so the caller can seed profiles for exactly those users (no
// profile is ever created for a user with no events).
func (s *Seeder) backfillEvents(ctx context.Context, projectID string, count int64, batchSize int) (map[int]struct{}, error) {
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

	active := make(map[int]struct{})
	var inserted int64
	startTime := time.Now()

	for inserted < count {
		select {
		case <-ctx.Done():
			slog.InfoContext(ctx, "seed interrupted", slog.Int64("inserted", inserted))
			return nil, ctx.Err()
		default:
		}

		size := min(int64(batchSize), count-inserted)
		// Refresh each batch so re-anchoring and the insert clamp use live time, not
		// a stale seed-start instant (long runs can otherwise leave a recent dead zone).
		end := time.Now()

		n, err := s.insertBatch(ctx, projectID, factory, int(size), start, end, active)
		if err != nil {
			return nil, fmt.Errorf("batch insert failed at offset %d: %w", inserted, err)
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
		slog.Int("active_users", len(active)),
		slog.String("elapsed", time.Since(startTime).Round(time.Second).String()),
	)
	return active, nil
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

// ProfileCount returns how many profiles are stored for projectID. The demo
// worker uses it to detect the events-present-but-profiles-missing state left by
// a seed that crashed after the event backfill but before profiles finished.
func ProfileCount(ctx context.Context, ch driver.Conn, projectID string) (uint64, error) {
	var n uint64
	if err := ch.QueryRow(ctx, "SELECT count() FROM profiles WHERE project_id = ?", projectID).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// BackfillEvents appends `count` synthetic historical events for projectID
// (without truncating) and returns the ascending list of human user indices
// that produced at least one event. The demo worker pairs it with
// SeedProfilesForUsers + CopyProfilesToClickHouse so a profile is created only
// for a user who has events. It needs only a ClickHouse connection.
func BackfillEvents(ctx context.Context, ch driver.Conn, projectID string, count int64, batchSize int) ([]int, error) {
	s := &Seeder{deps: &deps{ch: ch}}
	active, err := s.backfillEvents(ctx, projectID, count, batchSize)
	if err != nil {
		return nil, err
	}
	indices := make([]int, 0, len(active))
	for i := range active {
		indices = append(indices, i)
	}
	sort.Ints(indices)
	return indices, nil
}

// CopyProfilesToClickHouse copies the project's Postgres profiles into
// ClickHouse (the profiles read API is CH-backed) without truncating. The demo
// worker calls it after the active-set profiles have been written to Postgres,
// so only users with events are copied.
func CopyProfilesToClickHouse(ctx context.Context, pg *pgxpool.Pool, ch driver.Conn, projectID string) error {
	s := &Seeder{deps: &deps{pg: pg, ch: ch}}
	return s.runProfiles(ctx, projectID)
}

// InsertLiveEvent writes a single generated live event straight into the
// ClickHouse events table using the exact path the backfill uses (promoted
// column split + variant typing), so the live demo worker can stream traffic
// without the NATS ingestion hop. The demo worker paces these inserts to each
// event's occur time, so the live feed updates in real time.
func InsertLiveEvent(ctx context.Context, ch driver.Conn, projectID string, e LiveEvent) error {
	batch, err := ch.PrepareBatch(ctx, chq.EventsInsertStmt)
	if err != nil {
		return fmt.Errorf("prepare events batch: %w", err)
	}
	promoted, restAuto := chq.SplitPromotedAutoAnyProperties(e.AutoProperties)
	args := []any{
		e.EventID,
		projectID,
		e.DistinctID,
		e.Kind,
		autoAnyMapToVariantMap(ctx, projectID, restAuto),
		autoAnyMapToVariantMap(ctx, projectID, e.CustomProperties),
	}
	args = append(args, promoted.AppendArgs()...)
	args = append(args, e.OccurTime, e.SessionID)
	if err := batch.Append(args...); err != nil {
		return fmt.Errorf("append event: %w", err)
	}
	if err := batch.Send(); err != nil {
		return fmt.Errorf("send events batch: %w", err)
	}
	return nil
}

// LiveProfile is the payload for InsertLiveProfile — the direct-write
// counterpart of a backfilled profile row. Named fields keep create/update
// time (and the id/external-id strings) from being swapped at the call site.
type LiveProfile struct {
	ID         string
	ExternalID string // "" == anonymous
	Properties map[string]any
	CreateTime time.Time
	UpdateTime time.Time
}

// InsertLiveProfile writes a single profile straight into ClickHouse — the
// direct-write counterpart of CopyProfilesToClickHouse, without Postgres. The
// demo worker uses it to create a profile the first time it emits live events
// for a user, so a profile only ever exists for a user that has events.
func InsertLiveProfile(ctx context.Context, ch driver.Conn, projectID string, p LiveProfile) error {
	propsJSON, err := json.Marshal(p.Properties)
	if err != nil {
		return fmt.Errorf("marshal properties: %w", err)
	}
	batch, err := ch.PrepareBatch(ctx, chq.ProfilesInsertStmt)
	if err != nil {
		return fmt.Errorf("prepare profiles batch: %w", err)
	}
	if err := batch.Append(p.ID, projectID, p.ExternalID, string(propsJSON), uint8(0), p.CreateTime, p.UpdateTime); err != nil {
		return fmt.Errorf("append profile: %w", err)
	}
	if err := batch.Send(); err != nil {
		return fmt.Errorf("send profiles batch: %w", err)
	}
	return nil
}

func (s *Seeder) insertBatch(ctx context.Context, projectID string, factory *sessionFactory, size int, start, end time.Time, active map[int]struct{}) (int, error) {
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
		before := inserted
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
		// Record the human user behind this session so the caller seeds a profile
		// for them — but only if at least one of their events was actually
		// appended (a session truncated to zero rows at the batch cap must not
		// count it as active). recordActiveUser ignores bot ids.
		if inserted > before {
			recordActiveUser(active, sess[0].distinctID)
		}
	}

	return inserted, batch.Send()
}

// recordActiveUser marks the human user behind a session as having produced
// events, so the caller seeds a profile for exactly that user. Bot/non-user
// distinct ids are ignored (they never get a profile). The nil check is a
// defensive no-op; all current callers pass a non-nil set.
func recordActiveUser(active map[int]struct{}, distinctID string) {
	if active == nil {
		return
	}
	if idx, ok := HumanUserIndex(distinctID); ok {
		active[idx] = struct{}{}
	}
}

// HumanUserIndex extracts the integer index i from a generator-emitted human
// distinct id ("user-%05d"). Bot sessions ("bot-%04d"), junk, and any index
// outside the pool return ok=false so they never get a profile. Because ok=true
// guarantees i is in [0, DistinctIDPool), the result feeds DemoUserAt without
// risking its out-of-range panic. The format is owned by demoUserProfile. The
// live worker uses it to map a session's distinct id back to its demo user.
func HumanUserIndex(distinctID string) (int, bool) {
	rest, ok := strings.CutPrefix(distinctID, "user-")
	if !ok {
		return 0, false
	}
	i, err := strconv.Atoi(rest)
	if err != nil || i < 0 || i >= DistinctIDPool {
		return 0, false
	}
	return i, true
}

func (s *Seeder) runProfiles(ctx context.Context, projectID string) error {
	slog.InfoContext(ctx, "copying profiles from PostgreSQL to ClickHouse",
		slog.String("project_id", projectID),
	)

	pgRead := dbread.New(s.deps.pg)
	profiles, err := pgRead.GetAllProfilesByProjectID(ctx, projectID)
	if err != nil {
		return fmt.Errorf("query profiles: %w", err)
	}

	batch, err := s.deps.ch.PrepareBatch(ctx, chq.ProfilesInsertStmt)
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
			batch, err = s.deps.ch.PrepareBatch(ctx, chq.ProfilesInsertStmt)
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
