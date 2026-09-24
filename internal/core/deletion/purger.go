package deletion

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// These are the physical project-bearing ClickHouse tables, including the
// targets of materialized views. Keep this list in step with CH migrations.
var clickhouseProjectTables = [...]string{
	"events", "profile_aliases", "profiles", "event_names",
	"property_keys_event_buckets", "property_keys_profile_current",
	"distinct_id_activity_states", "dashboard_event_rollup_daily",
	"dashboard_session_rollup",
}

var refreshViews = [...]string{
	"property_keys_event_buckets_mv", "property_keys_profile_current_mv",
}

var ErrPurgerBusy = errors.New("another deletion purge is in progress")

type NotDueError struct {
	Delay time.Duration
}

func (e *NotDueError) Error() string {
	return "deletion cancellation window has not elapsed"
}

type Purger struct {
	pg       *pgxpool.Pool
	replicas []driver.Conn
}

func NewPurger(pg *pgxpool.Pool, ch driver.Conn, replicas ...driver.Conn) *Purger {
	return &Purger{pg: pg, replicas: append([]driver.Conn{ch}, replicas...)}
}

// ProcessOperation executes one NATS-delivered deletion operation. PostgreSQL
// remains the source of truth for status and progress; the message is only the
// durable dispatch signal. A session lock serializes the global ClickHouse view
// pause across worker replicas, and NATS retries when another purge owns it.
func (p *Purger) ProcessOperation(ctx context.Context, id string) error {
	conn, err := p.pg.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	var acquired bool
	if err := conn.QueryRow(ctx, `select pg_try_advisory_lock(hashtext('pug-deletion-purger'))`).Scan(&acquired); err != nil {
		return err
	}
	if !acquired {
		return ErrPurgerBusy
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `select pg_advisory_unlock(hashtext('pug-deletion-purger'))`)
	}()
	claimed, err := p.claim(ctx, conn, id)
	if err != nil {
		return err
	}
	if !claimed {
		return nil
	}
	if err := p.verifyReplicas(ctx); err != nil {
		return fmt.Errorf("verify replicas: %w", err)
	}

	// A prior worker could have died after pausing a refreshable view. Recovery
	// runs only while holding the global lock, so it never resumes another
	// worker's active purge.
	for _, replica := range p.replicas {
		for _, view := range refreshViews {
			if err := replica.Exec(ctx, "SYSTEM START VIEW "+view); err != nil {
				return fmt.Errorf("resume %s: %w", view, err)
			}
		}
	}

	if err := p.process(ctx, id); err != nil {
		return fmt.Errorf("purge %s: %w", id, err)
	}
	return nil
}

func (p *Purger) claim(ctx context.Context, conn *pgxpool.Conn, id string) (bool, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var status string
	var purgeAfter, databaseNow time.Time
	if err := tx.QueryRow(ctx, `select status,purge_after,clock_timestamp() from deletion_operations where id=$1 for update`, id).Scan(&status, &purgeAfter, &databaseNow); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrNotFound
		}
		return false, err
	}
	if status == "pending_deletion" && purgeAfter.After(databaseNow) {
		return false, &NotDueError{Delay: purgeAfter.Sub(databaseNow)}
	}
	if status != "pending_deletion" && status != "deleting" {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `update deletion_operations set status='deleting',started_at=coalesce(started_at,now()) where id=$1`, id); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `update projects set deletion_state='deleting' where id in (select project_id from deletion_project_steps where operation_id=$1) and deletion_state='pending_deletion'`, id); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `update orgs set deletion_state='deleting' where id=(select target_id from deletion_operations where id=$1 and target_type='organization') and deletion_state='pending_deletion'`, id); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func (p *Purger) process(ctx context.Context, id string) (resultErr error) {
	var kind, orgID string
	if err := p.pg.QueryRow(ctx, `select target_type,org_id from deletion_operations where id=$1`, id).Scan(&kind, &orgID); err != nil {
		return err
	}
	// The subscription may have changed during the cancellation window.
	// Fail before the first irreversible ClickHouse mutation.
	if kind == "organization" {
		if err := checkBilling(ctx, p.pg, orgID); err != nil {
			return err
		}
	}
	paused := true
	defer func() {
		if !paused {
			return
		}
		resumeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		for _, replica := range p.replicas {
			for _, view := range refreshViews {
				if err := replica.Exec(resumeCtx, "SYSTEM START VIEW "+view); err != nil && resultErr == nil {
					resultErr = fmt.Errorf("resume %s: %w", view, err)
				}
			}
		}
	}()
	for _, replica := range p.replicas {
		for _, view := range refreshViews {
			if err := replica.Exec(ctx, "SYSTEM PAUSE VIEW "+view); err != nil {
				return fmt.Errorf("pause %s: %w", view, err)
			}
		}
	}
	for _, replica := range p.replicas {
		for _, view := range refreshViews {
			if err := replica.Exec(ctx, "SYSTEM WAIT VIEW "+view); err != nil {
				return fmt.Errorf("wait %s: %w", view, err)
			}
		}
	}

	rows, err := p.pg.Query(ctx, `select project_id,clickhouse_done_at,postgres_done_at from deletion_project_steps where operation_id=$1 order by project_id`, id)
	if err != nil {
		return err
	}
	type step struct {
		projectID      string
		chDone, pgDone *time.Time
	}
	steps := []step{}
	for rows.Next() {
		var s step
		if err := rows.Scan(&s.projectID, &s.chDone, &s.pgDone); err != nil {
			rows.Close()
			return err
		}
		steps = append(steps, s)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, s := range steps {
		if s.chDone == nil {
			if err := p.purgeClickHouseProject(ctx, s.projectID); err != nil {
				return err
			}
			if _, err := p.pg.Exec(ctx, `update deletion_project_steps set clickhouse_done_at=now() where operation_id=$1 and project_id=$2`, id, s.projectID); err != nil {
				return err
			}
		}
		if s.pgDone == nil {
			tx, err := p.pg.Begin(ctx)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `delete from projects where id=$1 and deletion_state in ('deleting','failed')`, s.projectID); err != nil {
				_ = tx.Rollback(ctx)
				return err
			}
			if _, err := tx.Exec(ctx, `update deletion_project_steps set postgres_done_at=now() where operation_id=$1 and project_id=$2`, id, s.projectID); err != nil {
				_ = tx.Rollback(ctx)
				return err
			}
			if err := tx.Commit(ctx); err != nil {
				return err
			}
		}
	}
	// Resume refreshes before marking the operation complete. If a resume
	// fails, the operation remains retryable instead of recording "deleted" and
	// then overwriting it with "failed" after the transaction committed.
	for _, replica := range p.replicas {
		for _, view := range refreshViews {
			if err := replica.Exec(ctx, "SYSTEM START VIEW "+view); err != nil {
				return fmt.Errorf("resume %s: %w", view, err)
			}
		}
	}
	paused = false
	tx, err := p.pg.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var targetID, actorID string
	if err := tx.QueryRow(ctx, `select target_type,target_id,actor_id from deletion_operations where id=$1 for update`, id).Scan(&kind, &targetID, &actorID); err != nil {
		return err
	}
	if kind == "organization" {
		if err := checkBilling(ctx, tx, targetID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `delete from orgs where id=$1 and deletion_state in ('deleting','failed')`, targetID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `update deletion_operations set status='deleted',finished_at=now(),last_error='' where id=$1`, id); err != nil {
		return err
	}
	if err := audit(ctx, tx, actorID, kind+".deleted", kind, targetID, ""); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (p *Purger) purgeClickHouseProject(ctx context.Context, projectID string) error {
	for _, table := range clickhouseProjectTables {
		// A Replicated database can still contain non-replicated MergeTree tables.
		// Issue the mutation on every server, then verify every server below.
		for _, replica := range p.replicas {
			if err := replica.Exec(ctx, "ALTER TABLE "+table+" DELETE WHERE project_id = ? SETTINGS mutations_sync = 2", projectID); err != nil {
				return fmt.Errorf("delete %s: %w", table, err)
			}
		}
	}
	for _, table := range clickhouseProjectTables {
		for _, replica := range p.replicas {
			var remaining uint64
			if err := replica.QueryRow(ctx, "SELECT count() FROM "+table+" WHERE project_id = ?", projectID).Scan(&remaining); err != nil {
				return fmt.Errorf("verify %s: %w", table, err)
			}
			if remaining != 0 {
				return fmt.Errorf("verify %s: %d rows remain", table, remaining)
			}
		}
	}
	return nil
}

func (p *Purger) verifyReplicas(ctx context.Context) error {
	seen := map[string]bool{}
	var database, shard string
	expected := 1
	for _, replica := range p.replicas {
		var host, name, engine string
		if err := replica.QueryRow(ctx, `select hostName(),currentDatabase(),(select engine from system.databases where name=currentDatabase())`).Scan(&host, &name, &engine); err != nil {
			return err
		}
		if database != "" && database != name {
			return fmt.Errorf("deletion purge replica URLs use different databases: %s and %s", database, name)
		}
		database = name
		if seen[host] {
			return fmt.Errorf("deletion purge replica URLs resolve to the same server %s", host)
		}
		seen[host] = true
		var tableReplicas uint32
		if err := replica.QueryRow(ctx, `select max(total_replicas) from system.replicas where database=currentDatabase()`).Scan(&tableReplicas); err != nil {
			return err
		}
		if int(tableReplicas) > expected {
			expected = int(tableReplicas)
		}
		if engine == "Replicated" {
			var replicaShard string
			var databaseReplicas uint32
			if err := replica.QueryRow(ctx, `select shard_name,total_replicas from system.database_replicas where database=currentDatabase()`).Scan(&replicaShard, &databaseReplicas); err != nil {
				return err
			}
			if shard != "" && shard != replicaShard {
				return errors.New("deletion purge supports one ClickHouse shard only")
			}
			shard = replicaShard
			if int(databaseReplicas) > expected {
				expected = int(databaseReplicas)
			}
		}
	}
	if len(p.replicas) != expected {
		return fmt.Errorf("deletion purge requires a direct URL for every ClickHouse replica: configured %d, expected %d", len(p.replicas), expected)
	}
	return nil
}
