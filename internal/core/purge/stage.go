package purge

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"
	chq "github.com/pug-sh/pug/internal/core/clickhouse"
	"github.com/pug-sh/pug/internal/slogx"
)

var (
	ErrNoProject   = errors.New("purge: no project id")
	ErrLiveProject = errors.New("purge: project is live")
)

var tables = []Table{
	{Name: "events"},
	{Name: "profiles"},
	{Name: "profile_aliases"},
	{Name: "distinct_id_activity_states"},
	{Name: "dashboard_event_rollup_daily"},
	{Name: "dashboard_session_rollup"},
	{Name: "event_names"},
	{Name: "property_keys_event_buckets"},
	{Name: "property_keys_profile_current", Rebuilt: true},
}

// Filter selects the rows a delete removes. Retention would add a time bound.
type Filter struct {
	ProjectID string
}

func (f Filter) where() chq.Condition {
	return chq.Eq("project_id", f.ProjectID)
}

// Table is one ClickHouse table's state under a filter.
type Table struct {
	Name    string
	HasRows bool
	// A delete naming the project is queued or running.
	Deleting bool
	// The running delete's last failure. ClickHouse retries it on its own.
	FailReason string
	// Rebuilt from profiles every 5 minutes, so it is never deleted, only waited on.
	Rebuilt bool
}

// Check reports, per table, whether rows remain under f and whether a delete for
// the project is running.
func (s *Service) Check(ctx context.Context, f Filter) ([]Table, error) {
	if f.ProjectID == "" {
		return nil, s.failed(ctx, f.ProjectID, "check", ErrNoProject)
	}
	// Deletes before rows: one that finishes in between still reads as running.
	running, err := s.runningDeletes(ctx, f)
	if err != nil {
		return nil, err
	}
	out := make([]Table, 0, len(tables))
	for _, t := range tables {
		if t.HasRows, err = s.hasRows(ctx, t.Name, f); err != nil {
			return nil, err
		}
		t.FailReason, t.Deleting = running[t.Name]
		out = append(out, t)
	}
	return out, nil
}

// Start queues one delete on each table that still has rows under f, no delete
// running and no view rebuilding it, and returns the tables it queued. It never waits on ClickHouse.
func (s *Service) Start(ctx context.Context, f Filter) ([]string, error) {
	if err := s.refuseLive(ctx, f); err != nil {
		return nil, err
	}
	state, err := s.Check(ctx, f)
	if err != nil {
		return nil, err
	}
	where := f.where()
	var queued []string
	for _, t := range state {
		if t.Rebuilt || !t.HasRows || t.Deleting {
			continue
		}
		// The builder only writes SELECTs.
		query := "ALTER TABLE " + t.Name + " DELETE WHERE " + where.SQL() + " SETTINGS mutations_sync = 0"
		if err := s.ch.Exec(ctx, query, where.Args()...); err != nil {
			return queued, s.failed(ctx, f.ProjectID, "queue delete on "+t.Name, err)
		}
		queued = append(queued, t.Name)
	}
	return queued, nil
}

func (s *Service) refuseLive(ctx context.Context, f Filter) error {
	if f.ProjectID == "" {
		return s.failed(ctx, f.ProjectID, "start", ErrNoProject)
	}
	_, err := s.read.GetProjectByID(ctx, f.ProjectID)
	switch {
	case err == nil:
		return s.failed(ctx, f.ProjectID, "start", ErrLiveProject)
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	default:
		return s.failed(ctx, f.ProjectID, "check the project is not live", err)
	}
}

func (s *Service) runningDeletes(ctx context.Context, f Filter) (map[string]string, error) {
	// Matched on the project id anywhere in the command, so an erasure running
	// for the project also counts.
	query, args, err := chq.NewQuery().
		Select("table", "latest_fail_reason").
		From("system.mutations").
		Where(
			chq.RawCond("database = currentDatabase()"),
			chq.Eq("is_done", 0),
			chq.RawCond("position(command, ?) > 0", f.ProjectID),
		).
		Build()
	if err != nil {
		return nil, s.failed(ctx, f.ProjectID, "build the running deletes query", err)
	}
	rows, err := s.ch.Query(ctx, query, args...)
	if err != nil {
		return nil, s.failed(ctx, f.ProjectID, "read running deletes", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.WarnContext(ctx, "failed to close running deletes", slogx.Error(err))
		}
	}()
	out := map[string]string{}
	for rows.Next() {
		var name, reason string
		if err := rows.Scan(&name, &reason); err != nil {
			return nil, s.failed(ctx, f.ProjectID, "scan running deletes", err)
		}
		if out[name] == "" {
			out[name] = reason
		}
	}
	if err := rows.Err(); err != nil {
		return nil, s.failed(ctx, f.ProjectID, "read running deletes", err)
	}
	return out, nil
}

func (s *Service) hasRows(ctx context.Context, name string, f Filter) (bool, error) {
	query, args, err := chq.NewQuery().
		With("hit", chq.NewQuery().Select("1").From(name).Where(f.where()).Limit(1)).
		Select("count()").
		From("hit").
		Build()
	if err != nil {
		return false, s.failed(ctx, f.ProjectID, "build the row check for "+name, err)
	}
	var n uint64
	if err := s.ch.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		return false, s.failed(ctx, f.ProjectID, "check rows in "+name, err)
	}
	return n > 0, nil
}
