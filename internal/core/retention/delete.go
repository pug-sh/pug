package retention

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	chq "github.com/pug-sh/pug/internal/core/clickhouse"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/slogx"
)

// maxProjects bounds a statement's project ids, far under ClickHouse's 256 KiB
// max_query_size with the ids in it twice.
const maxProjects = 1000

// table is one ClickHouse table a cut deletes from.
type table struct {
	name string
	// before dates a row against the cut, for a table cut row by row.
	before string
	// A table of merged states is cut by key instead, since each part keeps its own
	// partial row per key and a delete reads rows one at a time. A key goes once
	// the max of its state over all its rows is before the cut.
	key, state string
}

var tables = []table{
	{name: "events", before: "occur_time < ?"},
	// toDate keeps the comparison on days, whatever the server's time zone.
	{name: "dashboard_event_rollup_daily", before: "day < toDate(?)"},
	{name: "property_keys_event_buckets", before: "bucket_time < ?"},
	// The keys leave out bot, and kind for sessions, since both split one session
	// or person over several rows.
	{name: "dashboard_session_rollup", key: "session_id", state: "end_state"},
	{name: "distinct_id_activity_states", key: "distinct_id", state: "last_seen_state"},
	{name: "event_names", key: "kind", state: "last_seen"},
}

// where selects the rows of the projects in ids dated before cut.
func (t table) where(ids []string, cut time.Time) (chq.Condition, error) {
	in := chq.In("project_id", ids)
	if t.key == "" {
		// And would drop an empty cut and keep only in: every row of the projects.
		if t.before == "" {
			return chq.Condition{}, errors.New("the table has no cut")
		}
		return chq.And(in, chq.RawCond(t.before, cut)), nil
	}
	keys, args, err := chq.NewQuery().
		Select("project_id", t.key).
		From(t.name).
		Where(in).
		GroupBy("project_id", t.key).
		HavingExpr("maxMerge("+t.state+") < ?", cut).
		Build()
	if err != nil {
		return chq.Condition{}, err
	}
	return chq.And(in, chq.RawCond("(project_id, "+t.key+") IN ("+keys+")", args...)), nil
}

// deleteBefore cuts every live project at its org's cut, table by table. A table
// with any delete unfinished, a purge's or an erasure's too, waits for a later
// run, so a table never holds more than one run's deletes.
func (s *Service) deleteBefore(ctx context.Context, cuts map[string]time.Time) error {
	if len(cuts) == 0 {
		return nil
	}
	live, err := s.read.ListLiveProjects(ctx)
	if err != nil {
		return failed(ctx, "list live projects", err)
	}
	byCut := map[time.Time][]string{}
	orgOf := map[string]string{}
	for _, p := range live {
		if at, ok := cuts[p.OrgID]; ok {
			byCut[at] = append(byCut[at], p.ID)
			orgOf[p.ID] = p.OrgID
		}
	}
	if len(byCut) == 0 {
		return nil
	}
	unfinished, err := s.unfinished(ctx)
	if err != nil {
		return err
	}

	var errs []error
	for _, t := range tables {
		reason, busy := unfinished[t.name]
		switch {
		case reason != "":
			// ClickHouse retries it forever, so a warning would hide a table never cut.
			err := fmt.Errorf("%w on %s: %s", ErrDeleteFailing, t.name, reason)
			slog.ErrorContext(ctx, "retention skips a table with a failing delete", slogx.Error(err))
			telemetry.RecordError(ctx, err)
			errs = append(errs, err)
		case busy:
			slog.WarnContext(ctx, "retention skips a table with deletes unfinished", slog.String("table", t.name))
		default:
			if err := s.cutTable(ctx, t, byCut, orgOf); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// cutTable logs one table's rows before each cut, and deletes them with the
// switch on. Every check runs before the first delete is queued, so the deletes
// go in together and ClickHouse rewrites each part they touch once.
func (s *Service) cutTable(ctx context.Context, t table, byCut map[time.Time][]string, orgOf map[string]string) error {
	type due struct {
		cut  time.Time
		rows map[string]uint64
	}
	var queue []due
	for _, at := range slices.SortedFunc(maps.Keys(byCut), time.Time.Compare) {
		for ids := range slices.Chunk(byCut[at], maxProjects) {
			rows, err := s.rowsBefore(ctx, t, ids, at)
			if err != nil {
				return err
			}
			if len(rows) > 0 {
				queue = append(queue, due{at, rows})
			}
		}
	}

	msg := "retention would delete"
	if s.cfg.Enabled {
		msg = "retention deletes"
	}
	for _, d := range queue {
		ids := slices.Sorted(maps.Keys(d.rows))
		if s.cfg.Enabled {
			if err := s.queueDelete(ctx, t, ids, d.cut); err != nil {
				return err
			}
		}
		for _, id := range ids {
			slog.InfoContext(ctx, msg, slog.String("table", t.name), slog.String("org_id", orgOf[id]),
				slog.String("project_id", id), slog.Time("before", d.cut), slog.Uint64("rows", d.rows[id]))
		}
	}
	return nil
}

// rowsBefore counts each project's rows dated before cut, leaving out projects
// with none.
func (s *Service) rowsBefore(ctx context.Context, t table, ids []string, cut time.Time) (map[string]uint64, error) {
	where, err := t.where(ids, cut)
	if err != nil {
		return nil, failed(ctx, "build the row check for "+t.name, err)
	}
	query, args, err := chq.NewQuery().
		Select("project_id", "count()").
		From(t.name).
		Where(where).
		GroupBy("project_id").
		Build()
	if err != nil {
		return nil, failed(ctx, "build the row check for "+t.name, err)
	}
	rows, err := s.ch.Query(ctx, query, args...)
	if err != nil {
		return nil, failed(ctx, "check rows in "+t.name, err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.WarnContext(ctx, "failed to close a retention row check", slogx.Error(err))
		}
	}()
	out := map[string]uint64{}
	for rows.Next() {
		var id string
		var n uint64
		if err := rows.Scan(&id, &n); err != nil {
			return nil, failed(ctx, "scan rows in "+t.name, err)
		}
		out[id] = n
	}
	if err := rows.Err(); err != nil {
		return nil, failed(ctx, "check rows in "+t.name, err)
	}
	return out, nil
}

func (s *Service) queueDelete(ctx context.Context, t table, ids []string, cut time.Time) error {
	where, err := t.where(ids, cut)
	if err != nil {
		return failed(ctx, "build the delete for "+t.name, err)
	}
	// The builder only writes SELECTs.
	query := "ALTER TABLE " + t.name + " DELETE WHERE " + where.SQL() + " SETTINGS mutations_sync = 0"
	if err := s.ch.Exec(ctx, query, where.Args()...); err != nil {
		return failed(ctx, "queue a delete on "+t.name, err)
	}
	return nil
}

// unfinished maps each table with a mutation not done to why one is failing, or "".
func (s *Service) unfinished(ctx context.Context) (map[string]string, error) {
	query, args, err := chq.NewQuery().
		Select("table", "anyIf(latest_fail_reason, latest_fail_reason != '')").
		From("system.mutations").
		Where(chq.RawCond("database = currentDatabase()"), chq.Eq("is_done", 0)).
		GroupBy("table").
		Build()
	if err != nil {
		return nil, failed(ctx, "build the unfinished deletes query", err)
	}
	rows, err := s.ch.Query(ctx, query, args...)
	if err != nil {
		return nil, failed(ctx, "read unfinished deletes", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.WarnContext(ctx, "failed to close unfinished deletes", slogx.Error(err))
		}
	}()
	out := map[string]string{}
	for rows.Next() {
		var name, reason string
		if err := rows.Scan(&name, &reason); err != nil {
			return nil, failed(ctx, "scan unfinished deletes", err)
		}
		out[name] = reason
	}
	if err := rows.Err(); err != nil {
		return nil, failed(ctx, "read unfinished deletes", err)
	}
	return out, nil
}
