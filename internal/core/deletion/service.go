package deletion

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	natsdeps "github.com/pug-sh/pug/internal/deps/nats"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	workercompliancev1 "github.com/pug-sh/pug/internal/gen/proto/workers/compliance/v1"
	"github.com/pug-sh/pug/internal/slogx"
	"github.com/rs/xid"
	"google.golang.org/protobuf/proto"
)

var (
	ErrNotFound          = errors.New("deletion target not found")
	ErrAlreadyRequested  = errors.New("deletion already requested")
	ErrNameMismatch      = errors.New("project confirmation name does not match")
	ErrReasonRequired    = errors.New("deletion reason required")
	ErrComplianceActive  = errors.New("compliance requests must finish before deletion")
	ErrBillingActive     = errors.New("active billing subscription must be cancelled before deletion")
	ErrCannotCancel      = errors.New("deletion can no longer be cancelled")
	ErrCannotRetry       = errors.New("only failed deletions can be retried")
	ErrPublisherRequired = errors.New("deletion purge publisher is required")
)

const OrganizationCancellationWindow = 24 * time.Hour

type Operation struct {
	ID         string
	TargetType string
	TargetID   string
	TargetName string
	OrgID      string
	ActorID    string
	ActorEmail string
	Reason     string
	Status     string
	Requested  time.Time
	PurgeAfter time.Time
	Started    *time.Time
	Finished   *time.Time
	LastError  string
	Projects   []ProjectStep
}

type ProjectStep struct {
	ProjectID      string
	ProjectName    string
	ClickHouseDone *time.Time
	PostgresDone   *time.Time
}

type ProjectKeyInvalidator interface {
	BlockProjectKeys(context.Context, string, ...string) error
	UnblockProjectKeys(context.Context, string, ...string) error
}

type Publisher interface {
	Publish(context.Context, string, []byte) error
}

type Service struct {
	pg             *pgxpool.Pool
	keyInvalidator ProjectKeyInvalidator
	publisher      Publisher
}

func NewServiceWithPublisher(pg *pgxpool.Pool, invalidator ProjectKeyInvalidator, publisher Publisher) *Service {
	return &Service{pg: pg, keyInvalidator: invalidator, publisher: publisher}
}

func NewService(pg *pgxpool.Pool, invalidators ...ProjectKeyInvalidator) *Service {
	s := &Service{pg: pg}
	if len(invalidators) > 0 {
		s.keyInvalidator = invalidators[0]
	}
	return s
}

func (s *Service) RequestProject(ctx context.Context, actorID, orgID, projectID, confirmationName string) (Operation, error) {
	var result Operation
	if s.publisher == nil {
		return result, ErrPublisherRequired
	}
	tx, err := s.pg.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockProjectExclusive(ctx, tx, projectID); err != nil {
		return result, err
	}
	var name, projectState, orgState string
	err = tx.QueryRow(ctx, `select p.display_name,p.deletion_state,o.deletion_state from projects p join orgs o on o.id=p.org_id where p.id=$1 and p.org_id=$2 for update of p`, projectID, orgID).Scan(&name, &projectState, &orgState)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, ErrNotFound
	}
	if err != nil {
		return result, err
	}
	if name != confirmationName {
		return result, ErrNameMismatch
	}
	if projectState != "active" || orgState != "active" {
		if projectState != "active" {
			return s.existing(ctx, tx, "project", projectID)
		}
		return result, ErrAlreadyRequested
	}
	if err := checkCompliance(ctx, tx, []string{projectID}); err != nil {
		return result, err
	}
	keyTokens, err := s.projectKeyTokens(ctx, tx, []string{projectID})
	if err != nil {
		return result, err
	}
	result = Operation{ID: xid.New().String(), TargetType: "project", TargetID: projectID, TargetName: name, OrgID: orgID, ActorID: actorID, Status: "pending_deletion"}
	if _, err := tx.Exec(ctx, `insert into deletion_operations(id,target_type,target_id,target_name,org_id,actor_id,status,purge_after) values($1,'project',$2,$3,$4,$5,'pending_deletion',now())`, result.ID, projectID, name, orgID, actorID); err != nil {
		return Operation{}, err
	}
	if _, err := tx.Exec(ctx, `insert into deletion_project_steps(operation_id,project_id,project_name) values($1,$2,$3)`, result.ID, projectID, name); err != nil {
		return Operation{}, err
	}
	if _, err := tx.Exec(ctx, `update projects set deletion_state='pending_deletion' where id=$1`, projectID); err != nil {
		return Operation{}, err
	}
	// Project deletion has no cancellation window. Remove its credentials in the
	// same transaction; the project row remains as the durable purge target and
	// the foreign-key cascade becomes a no-op when that row is finally removed.
	if _, err := tx.Exec(ctx, `delete from api_keys where project_id=$1`, projectID); err != nil {
		return Operation{}, err
	}
	if err := audit(ctx, tx, actorID, "project.deletion_requested", "project", projectID, ""); err != nil {
		return Operation{}, err
	}
	if err := s.blockProjectKeys(ctx, keyTokens); err != nil {
		return Operation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		s.unblockProjectKeysDetached(ctx, keyTokens)
		return Operation{}, err
	}
	if err := s.publishPurge(ctx, result.ID); err != nil {
		return Operation{}, errors.Join(err, s.markFailed(ctx, result.ID, err))
	}
	return s.Get(ctx, result.ID)
}

func (s *Service) RequestOrganization(ctx context.Context, actorID, orgID, confirmationID, reason string) (Operation, error) {
	var result Operation
	if s.publisher == nil {
		return result, ErrPublisherRequired
	}
	if orgID != confirmationID {
		return result, ErrNameMismatch
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return result, ErrReasonRequired
	}
	tx, err := s.pg.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtext('pug-org-deletion'),hashtext($1::text))`, orgID); err != nil {
		return result, err
	}
	var name, state string
	err = tx.QueryRow(ctx, `select display_name,deletion_state from orgs where id=$1 for update`, orgID).Scan(&name, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, ErrNotFound
	}
	if err != nil {
		return result, err
	}
	if state != "active" {
		return s.existing(ctx, tx, "organization", orgID)
	}
	if err := checkBilling(ctx, tx, orgID); err != nil {
		return result, err
	}
	rows, err := tx.Query(ctx, `select id,display_name,deletion_state from projects where org_id=$1 order by id`, orgID)
	if err != nil {
		return result, err
	}
	steps := []ProjectStep{}
	for rows.Next() {
		var p ProjectStep
		var projectState string
		if err := rows.Scan(&p.ProjectID, &p.ProjectName, &projectState); err != nil {
			rows.Close()
			return result, err
		}
		if projectState != "active" {
			rows.Close()
			return result, ErrAlreadyRequested
		}
		steps = append(steps, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	for _, p := range steps {
		if err := lockProjectExclusive(ctx, tx, p.ProjectID); err != nil {
			return result, err
		}
	}
	var inactiveCount int
	if err := tx.QueryRow(ctx, `select count(*) from projects where org_id=$1 and deletion_state<>'active'`, orgID).Scan(&inactiveCount); err != nil {
		return result, err
	}
	if inactiveCount != 0 {
		return result, ErrAlreadyRequested
	}
	ids := make([]string, 0, len(steps))
	for _, p := range steps {
		ids = append(ids, p.ProjectID)
	}
	if err := checkCompliance(ctx, tx, ids); err != nil {
		return result, err
	}
	keyTokens, err := s.projectKeyTokens(ctx, tx, ids)
	if err != nil {
		return result, err
	}
	result = Operation{ID: xid.New().String(), TargetType: "organization", TargetID: orgID, TargetName: name, OrgID: orgID, ActorID: actorID, Reason: reason, Status: "pending_deletion", Projects: steps}
	if _, err := tx.Exec(ctx, `insert into deletion_operations(id,target_type,target_id,target_name,org_id,actor_id,reason,status,purge_after) values($1,'organization',$2,$3,$2,$4,$5,'pending_deletion',now()+interval '24 hours')`, result.ID, orgID, name, actorID, reason); err != nil {
		return Operation{}, err
	}
	for _, p := range steps {
		if _, err := tx.Exec(ctx, `insert into deletion_project_steps(operation_id,project_id,project_name) values($1,$2,$3)`, result.ID, p.ProjectID, p.ProjectName); err != nil {
			return Operation{}, err
		}
	}
	if _, err := tx.Exec(ctx, `update orgs set deletion_state='pending_deletion' where id=$1`, orgID); err != nil {
		return Operation{}, err
	}
	if _, err := tx.Exec(ctx, `update projects set deletion_state='pending_deletion' where org_id=$1`, orgID); err != nil {
		return Operation{}, err
	}
	if err := audit(ctx, tx, actorID, "organization.deletion_requested", "organization", orgID, reason); err != nil {
		return Operation{}, err
	}
	if err := s.blockProjectKeys(ctx, keyTokens); err != nil {
		return Operation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		s.unblockProjectKeysDetached(ctx, keyTokens)
		return Operation{}, err
	}
	if err := s.publishPurge(ctx, result.ID); err != nil {
		return Operation{}, errors.Join(err, s.markFailed(ctx, result.ID, err))
	}
	return s.Get(ctx, result.ID)
}

func (s *Service) publishPurge(ctx context.Context, operationID string) error {
	data, err := proto.Marshal(&workercompliancev1.ProjectPurgeMessage{OperationId: proto.String(operationID)})
	if err != nil {
		return fmt.Errorf("marshal project purge message: %w", err)
	}
	if err := s.publisher.Publish(ctx, natsdeps.ComplianceProjectPurgeSubject, data); err != nil {
		wrapped := fmt.Errorf("publish project purge message: %w", err)
		slog.ErrorContext(ctx, "failed publishing project purge message",
			slog.String("operation_id", operationID), slogx.Error(wrapped))
		telemetry.RecordError(ctx, wrapped)
		return wrapped
	}
	return nil
}

func (s *Service) projectKeyTokens(ctx context.Context, tx pgx.Tx, projectIDs []string) (map[string][]string, error) {
	if s.keyInvalidator == nil || len(projectIDs) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `select project_id,token from api_keys where project_id=any($1) order by project_id,id`, projectIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tokens := make(map[string][]string, len(projectIDs))
	for rows.Next() {
		var projectID, token string
		if err := rows.Scan(&projectID, &token); err != nil {
			return nil, err
		}
		tokens[projectID] = append(tokens[projectID], token)
	}
	return tokens, rows.Err()
}

func (s *Service) blockProjectKeys(ctx context.Context, tokens map[string][]string) error {
	if s.keyInvalidator == nil || len(tokens) == 0 {
		return nil
	}
	blocked := make(map[string][]string, len(tokens))
	for projectID, projectTokens := range tokens {
		if err := s.keyInvalidator.BlockProjectKeys(ctx, projectID, projectTokens...); err != nil {
			s.unblockProjectKeysDetached(ctx, blocked)
			return err
		}
		blocked[projectID] = projectTokens
	}
	return nil
}

func (s *Service) unblockProjectKeys(ctx context.Context, tokens map[string][]string) error {
	if s.keyInvalidator == nil || len(tokens) == 0 {
		return nil
	}
	var result error
	for projectID, projectTokens := range tokens {
		result = errors.Join(result, s.keyInvalidator.UnblockProjectKeys(ctx, projectID, projectTokens...))
	}
	return result
}

func (s *Service) unblockProjectKeysDetached(ctx context.Context, tokens map[string][]string) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = s.unblockProjectKeys(cleanupCtx, tokens)
}

func (s *Service) existing(ctx context.Context, tx pgx.Tx, kind, targetID string) (Operation, error) {
	var id string
	err := tx.QueryRow(ctx, `select id from deletion_operations where target_type=$1 and target_id=$2 and status in ('pending_deletion','deleting','failed')`, kind, targetID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Operation{}, ErrAlreadyRequested
	}
	if err != nil {
		return Operation{}, err
	}
	return s.Get(ctx, id)
}

func checkCompliance(ctx context.Context, tx pgx.Tx, projectIDs []string) error {
	if len(projectIDs) == 0 {
		return nil
	}
	var pending bool
	if err := tx.QueryRow(ctx, `select exists(select 1 from compliance_requests where project_id=any($1) and status <> 'completed')`, projectIDs).Scan(&pending); err != nil {
		return err
	}
	if pending {
		return ErrComplianceActive
	}
	return nil
}

func checkBilling(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, orgID string) error {
	var active bool
	if err := q.QueryRow(ctx, `select exists(select 1 from billing_subscriptions where org_id=$1 and status in ('active','past_due'))`, orgID).Scan(&active); err != nil {
		return err
	}
	if active {
		return ErrBillingActive
	}
	return nil
}

func audit(ctx context.Context, tx pgx.Tx, actor, action, kind, target, reason string) error {
	_, err := tx.Exec(ctx, `insert into instance_audit(id,actor_id,action,target_type,target_id,reason) values($1,$2,$3,$4,$5,$6)`, xid.New().String(), actor, action, kind, target, reason)
	return err
}

// MarkFailed records a terminal NATS delivery or enqueue failure while leaving
// the target inaccessible and available for an explicit retry.
func (s *Service) MarkFailed(ctx context.Context, id string, cause error) error {
	return s.markFailed(ctx, id, cause)
}

func (s *Service) markFailed(ctx context.Context, id string, cause error) error {
	message := cause.Error()
	if len(message) > 2000 {
		message = message[:2000]
	}
	tx, err := s.pg.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var kind, targetID, actorID, status string
	if err := tx.QueryRow(ctx, `select target_type,target_id,actor_id,status from deletion_operations where id=$1 for update`, id).Scan(&kind, &targetID, &actorID, &status); err != nil {
		return err
	}
	if status == "deleted" || status == "cancelled" {
		return nil
	}
	if _, err := tx.Exec(ctx, `update deletion_operations set status='failed',last_error=$2 where id=$1`, id, message); err != nil {
		return err
	}
	if kind == "organization" {
		if _, err := tx.Exec(ctx, `update orgs set deletion_state='failed' where id=$1`, targetID); err != nil {
			return err
		}
	} else {
		if _, err := tx.Exec(ctx, `update projects set deletion_state='failed' where id=$1`, targetID); err != nil {
			return err
		}
	}
	if err := audit(ctx, tx, actorID, kind+".deletion_failed", kind, targetID, message); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) CancelOrganization(ctx context.Context, actorID, operationID string) (Operation, error) {
	tx, err := s.pg.Begin(ctx)
	if err != nil {
		return Operation{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var orgID, kind, status string
	var deadline time.Time
	var started *time.Time
	err = tx.QueryRow(ctx, `select target_id,target_type,status,purge_after,started_at from deletion_operations where id=$1 for update`, operationID).Scan(&orgID, &kind, &status, &deadline, &started)
	if errors.Is(err, pgx.ErrNoRows) {
		return Operation{}, ErrNotFound
	}
	if err != nil {
		return Operation{}, err
	}
	var beforeDeadline bool
	if err := tx.QueryRow(ctx, `select clock_timestamp() < $1`, deadline).Scan(&beforeDeadline); err != nil {
		return Operation{}, err
	}
	cancellableStatus := status == "pending_deletion" || (status == "failed" && started == nil)
	if kind != "organization" || !cancellableStatus || !beforeDeadline {
		return Operation{}, ErrCannotCancel
	}
	rows, err := tx.Query(ctx, `select project_id from deletion_project_steps where operation_id=$1 order by project_id`, operationID)
	if err != nil {
		return Operation{}, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return Operation{}, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return Operation{}, err
	}
	for _, id := range ids {
		if err := lockProjectExclusive(ctx, tx, id); err != nil {
			return Operation{}, err
		}
	}
	keyTokens, err := s.projectKeyTokens(ctx, tx, ids)
	if err != nil {
		return Operation{}, err
	}
	if _, err := tx.Exec(ctx, `update orgs set deletion_state='active' where id=$1 and deletion_state in ('pending_deletion','failed')`, orgID); err != nil {
		return Operation{}, err
	}
	if _, err := tx.Exec(ctx, `update projects set deletion_state='active' where org_id=$1 and deletion_state in ('pending_deletion','failed')`, orgID); err != nil {
		return Operation{}, err
	}
	if _, err := tx.Exec(ctx, `update deletion_operations set status='cancelled',finished_at=now() where id=$1`, operationID); err != nil {
		return Operation{}, err
	}
	if err := audit(ctx, tx, actorID, "organization.deletion_cancelled", "organization", orgID, ""); err != nil {
		return Operation{}, err
	}
	if err := s.unblockProjectKeys(ctx, keyTokens); err != nil {
		return Operation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = s.blockProjectKeys(cleanupCtx, keyTokens)
		return Operation{}, err
	}
	return s.Get(ctx, operationID)
}

func (s *Service) Retry(ctx context.Context, actorID, operationID string) (Operation, error) {
	if s.publisher == nil {
		return Operation{}, ErrPublisherRequired
	}
	tx, err := s.pg.Begin(ctx)
	if err != nil {
		return Operation{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var kind, targetID, status string
	err = tx.QueryRow(ctx, `select target_type,target_id,status from deletion_operations where id=$1 for update`, operationID).Scan(&kind, &targetID, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return Operation{}, ErrNotFound
	}
	if err != nil {
		return Operation{}, err
	}
	if status != "failed" {
		return Operation{}, ErrCannotRetry
	}
	if kind == "organization" {
		if err := checkBilling(ctx, tx, targetID); err != nil {
			return Operation{}, err
		}
	}
	if _, err := tx.Exec(ctx, `update deletion_operations set status='pending_deletion',purge_after=case when target_type='organization' and started_at is null then greatest(purge_after,now()) else now() end,last_error='' where id=$1`, operationID); err != nil {
		return Operation{}, err
	}
	if kind == "organization" {
		if _, err := tx.Exec(ctx, `update orgs set deletion_state='pending_deletion' where id=$1`, targetID); err != nil {
			return Operation{}, err
		}
	} else {
		if _, err := tx.Exec(ctx, `update projects set deletion_state='pending_deletion' where id=$1`, targetID); err != nil {
			return Operation{}, err
		}
	}
	if err := audit(ctx, tx, actorID, kind+".deletion_retried", kind, targetID, ""); err != nil {
		return Operation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Operation{}, err
	}
	if err := s.publishPurge(ctx, operationID); err != nil {
		return Operation{}, errors.Join(err, s.markFailed(ctx, operationID, err))
	}
	return s.Get(ctx, operationID)
}

func (s *Service) Get(ctx context.Context, id string) (Operation, error) {
	o, err := scanOperation(s.pg.QueryRow(ctx, `select d.id,d.target_type,d.target_id,d.target_name,d.org_id,d.actor_id,coalesce(c.email,''),d.reason,d.status,d.requested_at,d.purge_after,d.started_at,d.finished_at,d.last_error from deletion_operations d left join customers c on c.id=d.actor_id where d.id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Operation{}, ErrNotFound
	}
	if err != nil {
		return Operation{}, err
	}
	operations := []Operation{o}
	if err := s.loadSteps(ctx, operations); err != nil {
		return Operation{}, err
	}
	return operations[0], nil
}

func scanOperation(row interface{ Scan(...any) error }) (Operation, error) {
	var o Operation
	err := row.Scan(&o.ID, &o.TargetType, &o.TargetID, &o.TargetName, &o.OrgID, &o.ActorID, &o.ActorEmail, &o.Reason, &o.Status, &o.Requested, &o.PurgeAfter, &o.Started, &o.Finished, &o.LastError)
	return o, err
}

func (s *Service) loadSteps(ctx context.Context, operations []Operation) error {
	if len(operations) == 0 {
		return nil
	}
	ids := make([]string, len(operations))
	index := make(map[string]int, len(operations))
	for i := range operations {
		ids[i] = operations[i].ID
		index[ids[i]] = i
	}
	rows, err := s.pg.Query(ctx, `select operation_id,project_id,project_name,clickhouse_done_at,postgres_done_at from deletion_project_steps where operation_id=any($1) order by operation_id,project_id`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var operationID string
		var p ProjectStep
		if err := rows.Scan(&operationID, &p.ProjectID, &p.ProjectName, &p.ClickHouseDone, &p.PostgresDone); err != nil {
			return err
		}
		operations[index[operationID]].Projects = append(operations[index[operationID]].Projects, p)
	}
	return rows.Err()
}

func (s *Service) List(ctx context.Context, orgID string, limit int) ([]Operation, error) {
	result, _, err := s.ListPage(ctx, orgID, limit, "")
	return result, err
}

func (s *Service) ListPage(ctx context.Context, orgID string, limit int, afterID string) ([]Operation, string, error) {
	return s.listPage(ctx, orgID, "", limit, afterID)
}

func (s *Service) ListProjectsPage(ctx context.Context, orgID string, limit int, afterID string) ([]Operation, string, error) {
	return s.listPage(ctx, orgID, "project", limit, afterID)
}

func (s *Service) listPage(ctx context.Context, orgID, targetType string, limit int, afterID string) ([]Operation, string, error) {
	if limit < 1 || limit > 100 {
		limit = 50
	}
	rows, err := s.pg.Query(ctx, `select d.id,d.target_type,d.target_id,d.target_name,d.org_id,d.actor_id,coalesce(c.email,''),d.reason,d.status,d.requested_at,d.purge_after,d.started_at,d.finished_at,d.last_error
		from deletion_operations d left join customers c on c.id=d.actor_id
		where ($1='' or d.org_id=$1) and ($2='' or d.target_type=$2)
		and ($3='' or (d.requested_at,d.id) < (select requested_at,id from deletion_operations where id=$3 and ($1='' or org_id=$1) and ($2='' or target_type=$2)))
		order by d.requested_at desc,d.id desc limit $4`, orgID, targetType, afterID, limit+1)
	if err != nil {
		return nil, "", err
	}
	result := []Operation{}
	for rows.Next() {
		o, err := scanOperation(rows)
		if err != nil {
			rows.Close()
			return nil, "", err
		}
		result = append(result, o)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(result) > limit {
		result = result[:limit]
		next = result[len(result)-1].ID
	}
	if err := s.loadSteps(ctx, result); err != nil {
		return nil, "", err
	}
	return result, next, nil
}

// RetryProject only accepts a project operation belonging to the already
// authorized organization. The target and organization on an operation never
// change, so Retry can perform the state transition after this check.
func (s *Service) RetryProject(ctx context.Context, actorID, orgID, operationID string) (Operation, error) {
	o, err := s.Get(ctx, operationID)
	if err != nil {
		return Operation{}, err
	}
	if o.OrgID != orgID || o.TargetType != "project" {
		return Operation{}, ErrNotFound
	}
	return s.Retry(ctx, actorID, operationID)
}
