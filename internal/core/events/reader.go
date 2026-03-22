package events

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"
	"github.com/fivebitsio/cotton/internal/slogx"
)

type Event struct {
	AutoProperties   map[string]string
	CustomProperties map[string]string
	DistinctID       string
	EventID          string
	InsertTime       time.Time
	Kind             string
	OccurTime        time.Time
	ProjectID        string
	SessionID        uuid.UUID
}

type Reader struct {
	ch driver.Conn
}

func NewReader(ch driver.Conn) *Reader {
	return &Reader{ch: ch}
}

// GetEventsByProfile returns all events for a profile, including events recorded
// under any of its alias IDs (anonymous profiles that were merged into it).
func (r *Reader) GetEventsByProfile(ctx context.Context, projectID, profileID string) ([]Event, error) {
	aliasIDs, err := r.getAliasIDs(ctx, projectID, profileID)
	if err != nil {
		return nil, fmt.Errorf("GetEventsByProfile: getAliasIDs failed for project %s: %w", projectID, err)
	}

	ids := append([]string{profileID}, aliasIDs...)

	rows, err := r.ch.Query(ctx,
		`SELECT auto_properties, custom_properties, distinct_id, event_id, insert_time, kind, occur_time, project_id, session_id
		 FROM events FINAL
		 WHERE project_id = ? AND distinct_id IN ?
		 ORDER BY occur_time DESC`,
		projectID, ids)
	if err != nil {
		return nil, fmt.Errorf("GetEventsByProfile: query failed for project %s: %w", projectID, err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.ErrorContext(ctx, "failed to close ClickHouse rows", slogx.Error(err))
		}
	}()

	var events []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(
			&e.AutoProperties,
			&e.CustomProperties,
			&e.DistinctID,
			&e.EventID,
			&e.InsertTime,
			&e.Kind,
			&e.OccurTime,
			&e.ProjectID,
			&e.SessionID,
		); err != nil {
			return nil, fmt.Errorf("GetEventsByProfile: scan failed for project %s: %w", projectID, err)
		}
		events = append(events, e)
	}

	return events, rows.Err()
}

func (r *Reader) getAliasIDs(ctx context.Context, projectID, profileID string) ([]string, error) {
	rows, err := r.ch.Query(ctx,
		`SELECT alias_id FROM profile_aliases FINAL
		 WHERE project_id = ? AND profile_id = ?`,
		projectID, profileID)
	if err != nil {
		return nil, fmt.Errorf("getAliasIDs: query failed for project %s profile %s: %w", projectID, profileID, err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.ErrorContext(ctx, "failed to close ClickHouse rows", slogx.Error(err))
		}
	}()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("getAliasIDs: scan failed for project %s profile %s: %w", projectID, profileID, err)
		}
		ids = append(ids, id)
	}

	return ids, rows.Err()
}
