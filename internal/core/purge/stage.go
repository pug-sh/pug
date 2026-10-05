// Package purge erases what a deleted project stored.
package purge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	chdb "github.com/pug-sh/pug/internal/deps/clickhouse"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/slogx"
)

var (
	ErrNoProject   = errors.New("purge: no project id")
	ErrLiveProject = errors.New("purge: project is live")
)

type table struct {
	name string
	// Refilled by a refreshable view, so the stage waits for it to empty.
	rebuilt bool
}

var tables = []table{
	{name: "events"},
	{name: "profiles"},
	{name: "profile_aliases"},
	{name: "distinct_id_activity_states"},
	{name: "dashboard_event_rollup_daily"},
	{name: "dashboard_session_rollup"},
	{name: "event_names"},
	{name: "property_keys_event_buckets"},
	{name: "property_keys_profile_current", rebuilt: true},
}

// Filter selects the rows a delete removes. Retention would add a time bound.
type Filter struct {
	ProjectID string
}

func (f Filter) where() (string, []any) {
	return "project_id = ?", []any{f.ProjectID}
}

// Table is one ClickHouse table's state under a filter.
type Table struct {
	Name    string
	HasRows bool
	// A delete naming the project is queued or running.
	Deleting bool
	// The running delete's last failure. ClickHouse retries it on its own.
	FailReason string
	Rebuilt    bool
}

type Service struct {
	ch   *chdb.Conn
	read *dbread.Queries
}

func NewService(pgW *pgxpool.Pool, ch *chdb.Conn) *Service {
	return &Service{ch: ch, read: dbread.New(pgW)}
}

// Check reports, per table, whether rows remain under f and whether a delete for
// the project is running.
func (s *Service) Check(ctx context.Context, f Filter) ([]Table, error) {
	if f.ProjectID == "" {
		return nil, s.fail(ctx, "check", f, ErrNoProject)
	}
	// Deletes before rows: one that finishes in between still reads as running.
	running, err := s.runningDeletes(ctx, f)
	if err != nil {
		return nil, err
	}
	out := make([]Table, 0, len(tables))
	for _, t := range tables {
		hasRows, err := s.hasRows(ctx, t.name, f)
		if err != nil {
			return nil, err
		}
		reason, deleting := running[t.name]
		out = append(out, Table{
			Name: t.name, HasRows: hasRows, Deleting: deleting, FailReason: reason, Rebuilt: t.rebuilt,
		})
	}
	return out, nil
}

// Start queues one delete on each table that still has rows under f and no delete
// running, and returns the tables it queued. It never waits on ClickHouse.
func (s *Service) Start(ctx context.Context, f Filter) ([]string, error) {
	if err := s.refuseLive(ctx, f); err != nil {
		return nil, err
	}
	state, err := s.Check(ctx, f)
	if err != nil {
		return nil, err
	}
	where, args := f.where()
	var queued []string
	for _, t := range state {
		if t.Rebuilt || !t.HasRows || t.Deleting {
			continue
		}
		query := "ALTER TABLE " + t.Name + " DELETE WHERE " + where + " SETTINGS mutations_sync = 0"
		if err := s.ch.Exec(ctx, query, args...); err != nil {
			return queued, s.fail(ctx, "queue delete on "+t.Name, f, err)
		}
		queued = append(queued, t.Name)
	}
	return queued, nil
}

func (s *Service) refuseLive(ctx context.Context, f Filter) error {
	if f.ProjectID == "" {
		return s.fail(ctx, "start", f, ErrNoProject)
	}
	_, err := s.read.GetProjectByID(ctx, f.ProjectID)
	switch {
	case err == nil:
		return s.fail(ctx, "start", f, ErrLiveProject)
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	default:
		return s.fail(ctx, "check the project is not live", f, err)
	}
}

// Matched on the project id anywhere in the command, so an erasure running for
// the project also counts.
const runningDeletesQuery = `
SELECT table, latest_fail_reason
FROM system.mutations
WHERE database = currentDatabase() AND is_done = 0 AND position(command, ?) > 0
`

func (s *Service) runningDeletes(ctx context.Context, f Filter) (map[string]string, error) {
	rows, err := s.ch.Query(ctx, runningDeletesQuery, f.ProjectID)
	if err != nil {
		return nil, s.fail(ctx, "read running deletes", f, err)
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
			return nil, s.fail(ctx, "scan running deletes", f, err)
		}
		if prev, ok := out[name]; !ok || prev == "" {
			out[name] = reason
		}
	}
	if err := rows.Err(); err != nil {
		return nil, s.fail(ctx, "read running deletes", f, err)
	}
	return out, nil
}

func (s *Service) hasRows(ctx context.Context, name string, f Filter) (bool, error) {
	where, args := f.where()
	var n uint64
	query := "SELECT count() FROM (SELECT 1 FROM " + name + " WHERE " + where + " LIMIT 1)"
	if err := s.ch.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		return false, s.fail(ctx, "check rows in "+name, f, err)
	}
	return n > 0, nil
}

func (s *Service) fail(ctx context.Context, op string, f Filter, err error) error {
	err = fmt.Errorf("purge: %s: %w", op, err)
	slog.ErrorContext(ctx, "project purge failed", slogx.Error(err), slog.String("project_id", f.ProjectID))
	telemetry.RecordError(ctx, err)
	return err
}
