package profiles

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
)

type listProfilesParams struct {
	projectID  string
	hasCursor  bool
	cursorTime pgtype.Timestamptz
	cursorID   string
	pageSize   int32
	filter     sqlCondition
}

func listProfiles(ctx context.Context, db *pgxpool.Pool, params listProfilesParams) ([]dbread.Profile, error) {
	if db == nil {
		return nil, errors.New("profiles: read pool is nil")
	}

	sql, args := buildListProfilesQuery(params)
	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]dbread.Profile, 0)
	for rows.Next() {
		var p dbread.Profile
		if err := rows.Scan(
			&p.CreateTime,
			&p.DeletionTime,
			&p.ExternalID,
			&p.ID,
			&p.Properties,
			&p.ProjectID,
			&p.UpdateTime,
		); err != nil {
			return nil, err
		}
		items = append(items, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

func buildListProfilesQuery(params listProfilesParams) (string, []any) {
	sql := `select create_time, deletion_time, external_id, id, properties, project_id, update_time from profiles
where project_id = $1
  and deletion_time is null
  and (
    $2::bool = false
    or create_time < $3
    or (create_time = $3 and id < $4)
  )`
	args := []any{
		params.projectID,
		params.hasCursor,
		params.cursorTime,
		params.cursorID,
	}
	if !params.filter.isZero() {
		sql += "\n  and " + params.filter.sql
		args = append(args, params.filter.args...)
	}
	sql += fmt.Sprintf("\norder by create_time desc, id desc\nlimit $%d", len(args)+1)
	args = append(args, params.pageSize)
	return sql, args
}
