package orgemailproviders

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/pug-sh/pug/internal/app/server/rpc"
	coreemail "github.com/pug-sh/pug/internal/core/email"
	"github.com/pug-sh/pug/internal/core/email/secret"
	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	orgemailprovidersv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/orgemailproviders/v1"
	orgsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/orgs/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
)

type server struct {
	orgs   *coreorgs.Service
	read   *dbread.Queries
	write  *dbwrite.Queries
	cipher *secret.Cipher
	repo   *coreemail.OrgProviderRepo
	mailer *coreemail.Service // used by SendTest (Task 12)
}

// NewServer constructs the Get/Set/Remove handler. The cipher may be nil — in
// that case Get and Set return CodeFailedPrecondition until the operator
// configures an encryption key. The mailer is reserved for SendTest (Task 12)
// and is permitted to be nil here.
func NewServer(orgs *coreorgs.Service, read *dbread.Queries, write *dbwrite.Queries, cipher *secret.Cipher, repo *coreemail.OrgProviderRepo, mailer *coreemail.Service) *server {
	return &server{orgs: orgs, read: read, write: write, cipher: cipher, repo: repo, mailer: mailer}
}

// requireAdmin extracts the principal and verifies admin membership in the
// supplied org. Mirrors the gate in dashboard/orgs.handler.
func (s *server) requireAdmin(ctx context.Context, orgID string) error {
	principal, err := rpc.MustGetPrincipalWithCustomer(ctx)
	if err != nil {
		return connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}
	role, err := s.orgs.GetMemberRole(ctx, orgID, principal.Customer.ID)
	if err != nil {
		if errors.Is(err, coreorgs.ErrMemberNotFound) {
			return connect.NewError(connect.CodePermissionDenied, errors.New("not a member of this org"))
		}
		slog.ErrorContext(ctx, "failed to check org admin", slogx.Error(err),
			slog.String("org_id", orgID), slog.String("customer_id", principal.Customer.ID))
		telemetry.RecordError(ctx, err)
		return connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	if role != orgsv1.OrgRole_ORG_ROLE_ADMIN.String() {
		return connect.NewError(connect.CodePermissionDenied, errors.New("admin role required"))
	}
	return nil
}

// requireCipher rejects mutating/reading provider secrets when no encryption
// key is configured. Without a cipher the server cannot safely encrypt new
// secrets nor decrypt existing ones, so the only honest answer is
// FailedPrecondition with an operator-facing message.
func (s *server) requireCipher() error {
	if s.cipher == nil {
		return connect.NewError(connect.CodeFailedPrecondition,
			errors.New("email provider encryption key is not configured on this server"))
	}
	return nil
}

func (s *server) Get(ctx context.Context, req *connect.Request[orgemailprovidersv1.GetRequest]) (*connect.Response[orgemailprovidersv1.GetResponse], error) {
	if err := s.requireAdmin(ctx, req.Msg.GetOrgId()); err != nil {
		return nil, err
	}
	if err := s.requireCipher(); err != nil {
		return nil, err
	}

	row, err := s.read.GetOrgEmailProvider(ctx, req.Msg.GetOrgId())
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("no email provider configured for this org"))
		}
		slog.ErrorContext(ctx, "failed to get org email provider", slogx.Error(err),
			slog.String("org_id", req.Msg.GetOrgId()))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	plaintext, err := s.cipher.Decrypt(row.SecretCiphertext)
	if err != nil {
		slog.ErrorContext(ctx, "failed to decrypt org email provider", slogx.Error(err),
			slog.String("org_id", req.Msg.GetOrgId()))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	kind := coreemail.ProviderKind(row.Kind)
	redacted, err := redactPlaintext(kind, plaintext)
	if err != nil {
		slog.ErrorContext(ctx, "failed to redact org email provider", slogx.Error(err),
			slog.String("org_id", req.Msg.GetOrgId()))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	resp := &orgemailprovidersv1.GetResponse{
		Kind:           coreKindToProto(kind).Enum(),
		FromAddress:    &row.FromAddress,
		RedactedSecret: &redacted,
		UpdateTime:     timestamppb.New(row.UpdateTime.Time),
	}
	if row.ReplyTo.Valid {
		v := row.ReplyTo.String
		resp.ReplyTo = &v
	}
	return connect.NewResponse(resp), nil
}

func (s *server) Set(ctx context.Context, req *connect.Request[orgemailprovidersv1.SetRequest]) (*connect.Response[orgemailprovidersv1.SetResponse], error) {
	if err := s.requireAdmin(ctx, req.Msg.GetOrgId()); err != nil {
		return nil, err
	}
	if err := s.requireCipher(); err != nil {
		return nil, err
	}

	kind, cfg, err := configFromSetRequest(req.Msg)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid email provider config"))
	}

	plaintext, err := coreemail.EncodeProviderConfig(kind, cfg)
	if err != nil {
		slog.ErrorContext(ctx, "failed to encode provider config", slogx.Error(err),
			slog.String("org_id", req.Msg.GetOrgId()))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	ciphertext, err := s.cipher.Encrypt(plaintext)
	if err != nil {
		slog.ErrorContext(ctx, "failed to encrypt provider config", slogx.Error(err),
			slog.String("org_id", req.Msg.GetOrgId()))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	row, err := s.write.UpsertOrgEmailProvider(ctx, dbwrite.UpsertOrgEmailProviderParams{
		OrgID:            req.Msg.GetOrgId(),
		Kind:             string(kind),
		FromAddress:      req.Msg.GetFromAddress(),
		ReplyTo:          postgres.NewOptionalText(req.Msg.GetReplyTo()),
		SecretCiphertext: ciphertext,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to upsert org email provider", slogx.Error(err),
			slog.String("org_id", req.Msg.GetOrgId()))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	// Best-effort cache invalidation; failures are logged at source inside the
	// repo (Invalidate returns nothing).
	s.repo.Invalidate(ctx, req.Msg.GetOrgId())

	return connect.NewResponse(&orgemailprovidersv1.SetResponse{
		UpdateTime: timestamppb.New(row.UpdateTime.Time),
	}), nil
}

func (s *server) Remove(ctx context.Context, req *connect.Request[orgemailprovidersv1.RemoveRequest]) (*connect.Response[orgemailprovidersv1.RemoveResponse], error) {
	if err := s.requireAdmin(ctx, req.Msg.GetOrgId()); err != nil {
		return nil, err
	}
	if _, err := s.write.DeleteOrgEmailProvider(ctx, req.Msg.GetOrgId()); err != nil {
		slog.ErrorContext(ctx, "failed to delete org email provider", slogx.Error(err),
			slog.String("org_id", req.Msg.GetOrgId()))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	s.repo.Invalidate(ctx, req.Msg.GetOrgId())
	return connect.NewResponse(&orgemailprovidersv1.RemoveResponse{}), nil
}

// SendTest is implemented by Task 12. This stub keeps the
// OrgEmailProvidersServiceHandler interface satisfied so the server type can
// be wired into the dashboard mux as soon as Get/Set/Remove are ready.
func (s *server) SendTest(ctx context.Context, req *connect.Request[orgemailprovidersv1.SendTestRequest]) (*connect.Response[orgemailprovidersv1.SendTestResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("send test is not implemented"))
}
