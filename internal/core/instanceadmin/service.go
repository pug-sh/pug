package instanceadmin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pug-sh/pug/internal/core/instance"
	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	"github.com/pug-sh/pug/internal/core/projects"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
	"github.com/rs/xid"
)

var ErrLastAdmin = errors.New("cannot disable the last effective instance administrator")
var ErrNotFound = errors.New("resource not found")
var ErrInitialInvitationFailed = errors.New("organization created but initial invitation failed")
var ErrInvalidPageToken = errors.New("invalid page token")
var ErrOrgInactive = errors.New("organization is pending deletion")

type Membership struct {
	OrgID   string
	OrgName string
	Role    string
}
type User struct {
	ID          string
	Email       string
	CreatedAt   time.Time
	Verified    bool
	Disabled    bool
	Memberships []Membership
}
type UserFilters struct {
	Search   string
	OrgID    string
	Verified *bool
	Enabled  *bool
}
type Organization struct {
	ID            string
	Name          string
	CreatedAt     time.Time
	MemberCount   int32
	ProjectCount  int32
	AdminEmails   []string
	NeedsAdmin    bool
	DeletionState string
}
type Project struct {
	ID                string
	Name              string
	CreatedAt         time.Time
	ReportingTimezone string
	DeletionState     string
}
type Invitation struct {
	ID        string
	Email     string
	ExpiresAt time.Time
	Role      string
}
type OrganizationDetail struct {
	Organization            Organization
	Projects                []Project
	Members                 []User
	Invitations             []Invitation
	NextProjectPageToken    string
	NextMemberPageToken     string
	NextInvitationPageToken string
}

type Service struct {
	read   *pgxpool.Pool
	write  *pgxpool.Pool
	policy instance.Policy
	orgs   *coreorgs.Service
}

func NewService(read, write *pgxpool.Pool, policy instance.Policy, orgs *coreorgs.Service) *Service {
	return &Service{read: read, write: write, policy: policy, orgs: orgs}
}

func pageSize(n uint32) int32 {
	if n == 0 {
		return 50
	}
	if n > 100 {
		return 100
	}
	return int32(n)
}

func validCursor(cursor string) bool {
	if cursor == "" {
		return true
	}
	_, err := xid.FromString(cursor)
	return err == nil
}

func (s *Service) ListUsers(ctx context.Context, filters UserFilters, size uint32, cursor string) ([]User, string, error) {
	if !validCursor(cursor) {
		return nil, "", ErrInvalidPageToken
	}
	limit := pageSize(size)
	rows, err := s.read.Query(ctx, `select c.id, c.email, c.create_time, c.email_verified_at is not null, c.disabled_at is not null
		from customers c where c.id > $1
		and ($2 = '' or lower(c.email) like '%' || lower($2) || '%' or c.id = $2)
		and ($3 = '' or exists (select 1 from org_members m where m.customer_id = c.id and m.org_id = $3))
		and ($4::boolean is null or (c.email_verified_at is not null) = $4::boolean)
		and ($5::boolean is null or (c.disabled_at is null) = $5::boolean)
		order by c.id limit $6`, cursor, strings.TrimSpace(filters.Search), filters.OrgID, filters.Verified, filters.Enabled, limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	users := make([]User, 0, limit)
	for rows.Next() {
		var user User
		if err := rows.Scan(&user.ID, &user.Email, &user.CreatedAt, &user.Verified, &user.Disabled); err != nil {
			return nil, "", err
		}
		users = append(users, user)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(users) > int(limit) {
		users = users[:limit]
		next = users[len(users)-1].ID
	}
	if len(users) == 0 {
		return users, next, nil
	}
	userIndexes := make(map[string]int, len(users))
	userIDs := make([]string, len(users))
	for i := range users {
		userIndexes[users[i].ID] = i
		userIDs[i] = users[i].ID
		users[i].Memberships = []Membership{}
	}
	membershipRows, err := s.read.Query(ctx, `select m.customer_id, o.id, o.display_name, m.role
		from org_members m join orgs o on o.id=m.org_id
		where m.customer_id=any($1::varchar[]) order by m.customer_id, o.id`, userIDs)
	if err != nil {
		return nil, "", err
	}
	defer membershipRows.Close()
	for membershipRows.Next() {
		var userID string
		var m Membership
		if err := membershipRows.Scan(&userID, &m.OrgID, &m.OrgName, &m.Role); err != nil {
			return nil, "", err
		}
		users[userIndexes[userID]].Memberships = append(users[userIndexes[userID]].Memberships, m)
	}
	if err := membershipRows.Err(); err != nil {
		return nil, "", err
	}
	return users, next, nil
}

// GetUser returns one user by exact ID from the writer so mutation responses do
// not depend on fuzzy search ordering or read-replica lag.
func (s *Service) GetUser(ctx context.Context, id string) (User, error) {
	var user User
	err := s.write.QueryRow(ctx, `select id, email, create_time, email_verified_at is not null, disabled_at is not null
		from customers where id=$1`, id).Scan(&user.ID, &user.Email, &user.CreatedAt, &user.Verified, &user.Disabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return user, ErrNotFound
	}
	if err != nil {
		return user, err
	}
	rows, err := s.write.Query(ctx, `select o.id, o.display_name, m.role
		from org_members m join orgs o on o.id=m.org_id where m.customer_id=$1 order by o.id`, id)
	if err != nil {
		return user, err
	}
	defer rows.Close()
	user.Memberships = []Membership{}
	for rows.Next() {
		var membership Membership
		if err := rows.Scan(&membership.OrgID, &membership.OrgName, &membership.Role); err != nil {
			return user, err
		}
		user.Memberships = append(user.Memberships, membership)
	}
	return user, rows.Err()
}

func (s *Service) ListOrganizations(ctx context.Context, search string, size uint32, cursor string) ([]Organization, string, error) {
	if !validCursor(cursor) {
		return nil, "", ErrInvalidPageToken
	}
	limit := pageSize(size)
	rows, err := s.read.Query(ctx, `select o.id, o.display_name, o.create_time,
		(select count(*) from org_members where org_id=o.id)::int,
		(select count(*) from projects where org_id=o.id)::int,
		coalesce((select array_agg(c.email order by c.email) from org_members m join customers c on c.id=m.customer_id where m.org_id=o.id and m.role='ORG_ROLE_ADMIN'), array[]::varchar[]),
		not exists(select 1 from org_members m join customers c on c.id=m.customer_id where m.org_id=o.id and m.role='ORG_ROLE_ADMIN' and c.disabled_at is null), o.deletion_state
		from orgs o where o.id > $1 and ($2 = '' or lower(o.display_name) like '%' || lower($2) || '%' or o.id = $2)
		order by o.id limit $3`, cursor, strings.TrimSpace(search), limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	orgs := make([]Organization, 0, limit)
	for rows.Next() {
		var o Organization
		if err := rows.Scan(&o.ID, &o.Name, &o.CreatedAt, &o.MemberCount, &o.ProjectCount, &o.AdminEmails, &o.NeedsAdmin, &o.DeletionState); err != nil {
			return nil, "", err
		}
		orgs = append(orgs, o)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(orgs) > int(limit) {
		orgs = orgs[:limit]
		next = orgs[len(orgs)-1].ID
	}
	return orgs, next, nil
}

func (s *Service) GetOrganization(ctx context.Context, id string, pageSizeValue uint32, cursors ...string) (OrganizationDetail, error) {
	var result OrganizationDetail
	var projectCursor, memberCursor, invitationCursor string
	if len(cursors) > 0 {
		projectCursor = cursors[0]
	}
	if len(cursors) > 1 {
		memberCursor = cursors[1]
	}
	if len(cursors) > 2 {
		invitationCursor = cursors[2]
	}
	if !validCursor(projectCursor) || !validCursor(memberCursor) || !validCursor(invitationCursor) {
		return result, ErrInvalidPageToken
	}
	limit := pageSize(pageSizeValue)
	err := s.read.QueryRow(ctx, `select o.id, o.display_name, o.create_time,
		(select count(*) from org_members where org_id=o.id)::int,
		(select count(*) from projects where org_id=o.id)::int,
		coalesce((select array_agg(c.email order by c.email) from org_members m join customers c on c.id=m.customer_id where m.org_id=o.id and m.role='ORG_ROLE_ADMIN'), array[]::varchar[]),
		not exists(select 1 from org_members m join customers c on c.id=m.customer_id where m.org_id=o.id and m.role='ORG_ROLE_ADMIN' and c.disabled_at is null), o.deletion_state
		from orgs o where o.id=$1`, id).Scan(&result.Organization.ID, &result.Organization.Name,
		&result.Organization.CreatedAt, &result.Organization.MemberCount, &result.Organization.ProjectCount, &result.Organization.AdminEmails, &result.Organization.NeedsAdmin, &result.Organization.DeletionState)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, ErrNotFound
	}
	if err != nil {
		return result, err
	}
	projectRows, err := s.read.Query(ctx, `select id, display_name, create_time, reporting_timezone, deletion_state from projects where org_id=$1 and id > $2 order by id limit $3`, id, projectCursor, limit+1)
	if err != nil {
		return result, err
	}
	result.Projects = []Project{}
	for projectRows.Next() {
		var p Project
		if err := projectRows.Scan(&p.ID, &p.Name, &p.CreatedAt, &p.ReportingTimezone, &p.DeletionState); err != nil {
			projectRows.Close()
			return result, err
		}
		result.Projects = append(result.Projects, p)
	}
	err = projectRows.Err()
	projectRows.Close()
	if err != nil {
		return result, err
	}
	if len(result.Projects) > int(limit) {
		result.Projects = result.Projects[:limit]
		result.NextProjectPageToken = result.Projects[len(result.Projects)-1].ID
	}
	memberRows, err := s.read.Query(ctx, `select c.id, c.email, c.create_time, c.email_verified_at is not null, c.disabled_at is not null, m.role
		from org_members m join customers c on c.id=m.customer_id where m.org_id=$1 and c.id > $2 order by c.id limit $3`, id, memberCursor, limit+1)
	if err != nil {
		return result, err
	}
	result.Members = []User{}
	for memberRows.Next() {
		var u User
		var role string
		if err := memberRows.Scan(&u.ID, &u.Email, &u.CreatedAt, &u.Verified, &u.Disabled, &role); err != nil {
			memberRows.Close()
			return result, err
		}
		u.Memberships = []Membership{{OrgID: id, OrgName: result.Organization.Name, Role: role}}
		result.Members = append(result.Members, u)
	}
	err = memberRows.Err()
	memberRows.Close()
	if err != nil {
		return result, err
	}
	if len(result.Members) > int(limit) {
		result.Members = result.Members[:limit]
		result.NextMemberPageToken = result.Members[len(result.Members)-1].ID
	}
	inviteRows, err := s.read.Query(ctx, `select id, email, expires_at, role from org_invitations where org_id=$1 and status='INVITATION_STATUS_PENDING' and id > $2 order by id limit $3`, id, invitationCursor, limit+1)
	if err != nil {
		return result, err
	}
	result.Invitations = []Invitation{}
	for inviteRows.Next() {
		var inv Invitation
		if err := inviteRows.Scan(&inv.ID, &inv.Email, &inv.ExpiresAt, &inv.Role); err != nil {
			inviteRows.Close()
			return result, err
		}
		result.Invitations = append(result.Invitations, inv)
	}
	err = inviteRows.Err()
	inviteRows.Close()
	if len(result.Invitations) > int(limit) {
		result.Invitations = result.Invitations[:limit]
		result.NextInvitationPageToken = result.Invitations[len(result.Invitations)-1].ID
	}
	return result, err
}

func audit(ctx context.Context, tx pgx.Tx, actor, action, targetType, targetID string) error {
	_, err := tx.Exec(ctx, `insert into instance_audit(id,actor_id,action,target_type,target_id) values($1,$2,$3,$4,$5)`, xid.New().String(), actor, action, targetType, targetID)
	return err
}

// Organization mutations commit inside the existing org service so its cache
// invalidation and invitation delivery remain authoritative. Persist an audit
// attempt first. A crash after the mutation leaves a durable entry to reconcile.
func (s *Service) auditedOrgAction(ctx context.Context, actor, action, orgID string, mutate func() error) error {
	auditID := xid.New().String()
	command, err := s.write.Exec(ctx, `insert into instance_audit(id,actor_id,action,target_type,target_id,details)
		select $1,$2,$3,'organization',$4,'{"status":"started"}'::jsonb from orgs where id=$4`, auditID, actor, action, orgID)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return ErrNotFound
	}
	mutationErr := mutate()
	status := "completed"
	if mutationErr != nil {
		status = "failed"
	}
	auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, auditErr := s.write.Exec(auditCtx, `update instance_audit set details=jsonb_build_object('status',$2::text) where id=$1`, auditID, status); auditErr != nil {
		wrapped := fmt.Errorf("record instance audit outcome: %w", auditErr)
		slog.ErrorContext(auditCtx, "failed to record instance organization audit outcome", slogx.Error(wrapped),
			slog.String("audit_id", auditID), slog.String("action", action), slog.String("org_id", orgID), slog.String("outcome", status))
		telemetry.RecordError(auditCtx, wrapped)
	}
	if mutationErr != nil {
		return mutationErr
	}
	return nil
}

func (s *Service) InviteMember(ctx context.Context, actor, orgID, email string, role coreorgs.Role) (coreorgs.InviteDispatch, error) {
	var result coreorgs.InviteDispatch
	err := s.auditedOrgAction(ctx, actor, "organization.member_invited", orgID, func() error {
		var err error
		result, err = s.orgs.InviteMemberWithRole(ctx, orgID, actor, email, role)
		return err
	})
	return result, err
}

func (s *Service) ResendInvitation(ctx context.Context, actor, orgID, invitationID string) (coreorgs.InviteDispatch, error) {
	var result coreorgs.InviteDispatch
	err := s.auditedOrgAction(ctx, actor, "organization.invitation_resent", orgID, func() error {
		var err error
		result, err = s.orgs.ResendInvite(ctx, orgID, invitationID)
		return err
	})
	return result, err
}

func (s *Service) RevokeInvitation(ctx context.Context, actor, orgID, invitationID string) error {
	return s.auditedOrgAction(ctx, actor, "organization.invitation_revoked", orgID, func() error {
		return s.orgs.RevokeInvite(ctx, orgID, invitationID)
	})
}

func (s *Service) SetMemberRole(ctx context.Context, actor, orgID, userID string, role coreorgs.Role) error {
	return s.auditedOrgAction(ctx, actor, "organization.member_role_changed", orgID, func() error {
		_, err := s.orgs.UpdateMemberRole(ctx, orgID, userID, role)
		return err
	})
}

func (s *Service) RemoveMember(ctx context.Context, actor, orgID, userID string) error {
	return s.auditedOrgAction(ctx, actor, "organization.member_removed", orgID, func() error {
		return s.orgs.RemoveMemberSafe(ctx, orgID, userID)
	})
}

func (s *Service) SetUserDisabled(ctx context.Context, actor, userID string, disabled bool) error {
	tx, err := s.write.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Serialize concurrent attempts to disable different allowlisted admins.
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtext('pug-instance-admin-disable'))`); err != nil {
		return err
	}
	var email string
	var verified, wasDisabled bool
	err = tx.QueryRow(ctx, `select email, email_verified_at is not null, disabled_at is not null from customers where id=$1 for update`, userID).Scan(&email, &verified, &wasDisabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if disabled && !wasDisabled && verified && s.policy.AllowsAdmin(email) {
		var count int
		if err := tx.QueryRow(ctx, `select count(*) from customers where lower(email)=any($1) and email_verified_at is not null and disabled_at is null`, s.policy.AdminEmails()).Scan(&count); err != nil {
			return err
		}
		if count <= 1 {
			return ErrLastAdmin
		}
	}
	if _, err := tx.Exec(ctx, `update customers set disabled_at=case when $2 then now() else null end, session_version=session_version+1 where id=$1`, userID, disabled); err != nil {
		return err
	}
	if disabled {
		if _, err := tx.Exec(ctx, `update refresh_tokens set revoked_at=now() where customer_id=$1 and revoked_at is null`, userID); err != nil {
			return err
		}
	}
	action := "user.enabled"
	if disabled {
		action = "user.disabled"
	}
	if err := audit(ctx, tx, actor, action, "user", userID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) RevokeUserSessions(ctx context.Context, actor, userID string) error {
	tx, err := s.write.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	command, err := tx.Exec(ctx, `update customers set session_version=session_version+1 where id=$1`, userID)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(ctx, `update refresh_tokens set revoked_at=now() where customer_id=$1 and revoked_at is null`, userID); err != nil {
		return err
	}
	if err := audit(ctx, tx, actor, "user.sessions_revoked", "user", userID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) ProvisionOrganization(ctx context.Context, actor, name, adminEmail string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 150 {
		return "", errors.New("invalid organization name")
	}
	if strings.TrimSpace(adminEmail) == "" {
		return "", errors.New("initial admin email required")
	}
	tx, err := s.write.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	w := dbwrite.New(tx)
	org, err := w.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: xid.New().String(), DisplayName: name})
	if err != nil {
		return "", err
	}
	if _, err := projects.CreateProjectInTx(ctx, w, org.ID, "default", ""); err != nil {
		return "", err
	}
	if err := audit(ctx, tx, actor, "organization.provisioned", "organization", org.ID); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	// The invitation service commits and queues email independently; the org remains
	// visible with zero members if mail delivery fails, so an admin can retry.
	if _, err := s.InviteMember(ctx, actor, org.ID, adminEmail, coreorgs.RoleAdmin); err != nil {
		return org.ID, fmt.Errorf("%w: %w", ErrInitialInvitationFailed, err)
	}
	return org.ID, nil
}

func (s *Service) RenameOrganization(ctx context.Context, actor, id, name string) error {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 150 {
		return errors.New("invalid organization name")
	}
	tx, err := s.write.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock_shared(hashtext('pug-org-deletion'),hashtext($1::text))`, id); err != nil {
		return err
	}
	var state string
	if err := tx.QueryRow(ctx, `select deletion_state from orgs where id=$1`, id).Scan(&state); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if state != "active" {
		return ErrOrgInactive
	}
	command, err := tx.Exec(ctx, `update orgs set display_name=$2 where id=$1`, id, name)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err := audit(ctx, tx, actor, "organization.renamed", "organization", id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
