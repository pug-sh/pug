package projects

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
	"github.com/pug-sh/pug/internal/tzx"
	"github.com/rs/xid"
)

var (
	ErrAdminRequired    = errors.New("admin role required")
	ErrProjectNotFound  = errors.New("project not found")
	ErrProjectNameTaken = errors.New("a project with this name already exists in the org")
)

// projectNameUnique is the Postgres-auto-generated name of the
// (org_id, display_name) unique constraint declared in
// schema/postgres/migrations/005_create_projects.sql:11. Kept narrow on
// purpose: creating a project also inserts its starter key, so a generic
// UniqueViolation catch would report an api_keys.token collision as a name
// conflict.
const projectNameUnique = "projects_org_id_display_name_key"

// isUniqueViolationOn reports whether err is a Postgres unique-violation
// against the given constraint name.
func isUniqueViolationOn(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == pgerrcode.UniqueViolation && pgErr.ConstraintName == constraint
}

type Service struct {
	read  *dbread.Queries
	write *dbwrite.Queries
	pgW   *pgxpool.Pool // for the methods that need a tx of their own (CreateProject, CreateProjectAsAdmin)
	repo  *Repo
}

func NewService(pgRO *pgxpool.Pool, pgW *pgxpool.Pool, repo *Repo) *Service {
	return &Service{
		read:  dbread.New(pgRO),
		write: dbwrite.New(pgW),
		pgW:   pgW,
		repo:  repo,
	}
}

func (s *Service) DeleteProject(ctx context.Context, arg dbwrite.DeleteProjectParams) error {
	// Listed before the delete, and fatal if it fails: the project's api_keys rows
	// cascade away with it, so this is the only chance to learn the tokens its
	// cached row is reachable by. Deleting without them would leave every one of the
	// project's keys authenticating — against a project that no longer exists —
	// until apiKeyCacheTTL, with no way left to find the cache entries: the tokens
	// are gone from the DB and are deliberately never logged. Failing here costs
	// nothing by comparison, since the project is still there to delete on a retry.
	// (apiKeyTokens logs + records at source.)
	tokens, err := s.apiKeyTokens(ctx, arg.ID)
	if err != nil {
		return err
	}
	if _, err := s.write.DeleteProject(ctx, arg); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrProjectNotFound
		}
		return err
	}
	s.invalidateTokens(ctx, arg.ID, tokens...)
	return nil
}

func (s *Service) CreateProjectAsAdmin(ctx context.Context, orgID, customerID, displayName, reportingTimezone string) (dbwrite.Project, error) {
	tx, err := s.pgW.Begin(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to begin create project transaction", slogx.Error(err),
			slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return dbwrite.Project{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	w := dbwrite.New(tx)

	project, err := w.CreateProjectAsAdmin(ctx, dbwrite.CreateProjectAsAdminParams{
		ID:                xid.New().String(),
		OrgID:             orgID,
		CustomerID:        customerID,
		DisplayName:       displayName,
		ReportingTimezone: tzx.Coerce(ctx, reportingTimezone),
	})
	if err != nil {
		// The CTE checks org_members for admin role. ErrNoRows means the INSERT was
		// skipped because no admin row exists for this org_id + customer_id.
		if errors.Is(err, pgx.ErrNoRows) {
			return dbwrite.Project{}, ErrAdminRequired
		}
		if isUniqueViolationOn(err, projectNameUnique) {
			return dbwrite.Project{}, ErrProjectNameTaken
		}
		slog.ErrorContext(ctx, "failed to create project as admin", slogx.Error(err),
			slog.String("org_id", orgID), slog.String("customer_id", customerID))
		telemetry.RecordError(ctx, err)
		return dbwrite.Project{}, err
	}

	// A project must never commit without a key to send events with, hence the
	// shared transaction. CreateApiKeyInTx logs + records at source.
	if _, err := CreateApiKeyInTx(ctx, w, project.ID, KindPublic, ""); err != nil {
		return dbwrite.Project{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "failed to commit create project", slogx.Error(err),
			slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return dbwrite.Project{}, err
	}
	return project, nil
}

func (s *Service) CreateProject(ctx context.Context, orgID, displayName, reportingTimezone string) (dbwrite.Project, error) {
	tx, err := s.pgW.Begin(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to begin create project transaction", slogx.Error(err),
			slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return dbwrite.Project{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	project, err := CreateProjectInTx(ctx, dbwrite.New(tx), orgID, displayName, reportingTimezone)
	if err != nil {
		return dbwrite.Project{}, err // CreateProjectInTx logs + records at source
	}

	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "failed to commit create project", slogx.Error(err),
			slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return dbwrite.Project{}, err
	}
	return project, nil
}

// CreateProjectInTx is the shared body of Service.CreateProject, exposed so
// callers with an open transaction (e.g. signup creating a default project
// alongside the customer+org rows) can run the same inserts under their own tx.
//
// The handle should be tx-bound. A project and its starter public key are two
// statements now, so outside a transaction a failure between them commits a
// project that cannot send events and has no key to send them with.
// Service.CreateProject opens a tx for exactly that reason, and every other
// production caller already had one. (A test that only needs a project to exist
// can pass a pool handle; it just does not get the atomicity.)
func CreateProjectInTx(ctx context.Context, w *dbwrite.Queries, orgID, displayName, reportingTimezone string) (dbwrite.Project, error) {
	project, err := w.CreateProject(ctx, dbwrite.CreateProjectParams{
		ID:                xid.New().String(),
		OrgID:             orgID,
		DisplayName:       displayName,
		ReportingTimezone: tzx.Coerce(ctx, reportingTimezone),
	})
	if err != nil {
		if isUniqueViolationOn(err, projectNameUnique) {
			return dbwrite.Project{}, ErrProjectNameTaken
		}
		slog.ErrorContext(ctx, "failed to create project", slogx.Error(err),
			slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return dbwrite.Project{}, err
	}

	// A project must never commit without a key to send events with, hence the
	// shared transaction. CreateApiKeyInTx logs + records at source.
	if _, err := CreateApiKeyInTx(ctx, w, project.ID, KindPublic, ""); err != nil {
		return dbwrite.Project{}, err
	}
	return project, nil
}

func (s *Service) GetProjectByID(ctx context.Context, id string) (dbread.Project, error) {
	return s.read.GetProjectByID(ctx, id)
}

func (s *Service) GetProjectsByOrgID(ctx context.Context, orgID string) ([]dbread.Project, error) {
	return s.read.GetProjectsByOrgID(ctx, orgID)
}

func (s *Service) ProjectExistsForOrgMember(ctx context.Context, projectID string, customerID string) (bool, error) {
	if _, err := s.read.GetProjectByIDAndOrgMember(ctx, dbread.GetProjectByIDAndOrgMemberParams{
		ID:         projectID,
		CustomerID: customerID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *Service) UpdateProjectMeta(ctx context.Context, arg dbwrite.UpdateProjectMetaParams) (dbwrite.Project, error) {
	project, err := s.write.UpdateProjectMeta(ctx, arg)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return dbwrite.Project{}, ErrProjectNotFound
		}
		return dbwrite.Project{}, err
	}
	s.invalidateProject(ctx, project)
	return project, nil
}

func (s *Service) UpdateFCMServiceJSON(ctx context.Context, arg dbwrite.UpdateFCMServiceJSONParams) (dbwrite.Project, error) {
	project, err := s.write.UpdateFCMServiceJSON(ctx, arg)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return dbwrite.Project{}, ErrProjectNotFound
		}
		return dbwrite.Project{}, err
	}
	s.invalidateProject(ctx, project)
	return project, nil
}

func (s *Service) invalidateProject(ctx context.Context, project dbwrite.Project) {
	// Detached here as well as inside invalidateTokens: the token listing is part
	// of this post-commit work, and on the caller's cancelled context it would
	// fail — invalidating none of the project's keys. Unlike DeleteProject, which
	// lists before its write, there is nothing to list until the update has landed.
	ctx, cancel := detachedInvalidateCtx(ctx)
	defer cancel()

	// Best-effort, unlike DeleteProject: the update has already committed, so there
	// is nothing to abort, and the project and its keys are still there to be listed
	// again. What goes stale here is a cached copy of the project's own metadata,
	// not a credential's validity. (apiKeyTokens logs + records at source.)
	tokens, err := s.apiKeyTokens(ctx, project.ID)
	if err != nil {
		return
	}
	s.invalidateTokens(ctx, project.ID, tokens...)
}
