package orgemailproviders

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/rs/xid"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/pug-sh/pug/internal/app/server/rpc"
	"github.com/pug-sh/pug/internal/apperr"
	"github.com/pug-sh/pug/internal/core/authz"
	coreemail "github.com/pug-sh/pug/internal/core/email"
	"github.com/pug-sh/pug/internal/core/email/secret"
	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	orgemailprovidersv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/orgemailproviders/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
)

type server struct {
	orgs       *coreorgs.Service
	read       *dbread.Queries
	write      *dbwrite.Queries
	cipher     *secret.Cipher
	repo       *coreemail.OrgProviderRepo
	mailer     *coreemail.Service
	authorizer *authz.Authorizer
}

// NewServer accepts nil cipher/repo/mailer for operator-only mode (no
// PUG_EMAIL_PROVIDER_SECRET_KEY). In that mode Get/Set return
// FailedPrecondition via requireCipher and SendTest returns the same on nil
// mailer.
func NewServer(orgs *coreorgs.Service, read *dbread.Queries, write *dbwrite.Queries, cipher *secret.Cipher, repo *coreemail.OrgProviderRepo, mailer *coreemail.Service, authorizer *authz.Authorizer) *server {
	return &server{orgs: orgs, read: read, write: write, cipher: cipher, repo: repo, mailer: mailer, authorizer: authorizer}
}

// requireAdmin authorizes org email-provider administration (email_provider is
// admin-only in the policy: members hold no email_provider permission, so any
// action denies a non-admin member with ORG_ADMIN_REQUIRED and a non-member with
// ORG_NOT_A_MEMBER — identical to the prior hand-rolled check).
func (s *server) requireAdmin(ctx context.Context, orgID string) (*rpc.Principal, error) {
	return rpc.RequirePermission(ctx, s.authorizer, s.orgs, orgID,
		authz.ResourceEmailProvider, authz.ActionUpdate,
		apperr.ReasonOrgAdminRequired, "admin role required")
}

// requireCipher returns FailedPrecondition when no encryption key is
// configured; without it the server can neither encrypt new secrets nor
// decrypt existing ones.
func (s *server) requireCipher() error {
	if s.cipher == nil {
		return apperr.FailedPrecondition(apperr.ReasonEmailProviderEncryptionMissing,
			"email provider encryption key is not configured on this server")
	}
	return nil
}

func (s *server) Get(ctx context.Context, req *connect.Request[orgemailprovidersv1.GetRequest]) (*connect.Response[orgemailprovidersv1.GetResponse], error) {
	if _, err := s.requireAdmin(ctx, req.Msg.GetOrgId()); err != nil {
		return nil, err
	}
	if err := s.requireCipher(); err != nil {
		return nil, err
	}

	row, err := s.read.GetOrgEmailProvider(ctx, req.Msg.GetOrgId())
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperr.NotFound(apperr.ReasonEmailProviderNotFound, "no email provider configured for this org",
				apperr.Resource("email_provider", req.Msg.GetOrgId()))
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
		// Decrypt failure usually means PUG_EMAIL_PROVIDER_SECRET_KEY was
		// rotated without re-encrypting rows. The admin can self-recover
		// by re-saving the provider config (which re-encrypts under the
		// current key) — surface a typed code rather than a generic 500.
		return nil, apperr.FailedPrecondition(apperr.ReasonEmailProviderDecryptFailed,
			"email provider secret cannot be decrypted with the current key; please re-save the provider config")
	}
	kind := coreemail.ProviderKind(row.Kind)
	redacted, err := redactPlaintext(kind, plaintext)
	if err != nil {
		slog.ErrorContext(ctx, "failed to redact org email provider", slogx.Error(err),
			slog.String("org_id", req.Msg.GetOrgId()))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	protoKind, err := coreKindToProto(kind)
	if err != nil {
		slog.ErrorContext(ctx, "unknown provider kind in db row", slogx.Error(err),
			slog.String("org_id", req.Msg.GetOrgId()), slog.String("kind", string(kind)))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	resp := &orgemailprovidersv1.GetResponse{
		Kind:           protoKind.Enum(),
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
	if _, err := s.requireAdmin(ctx, req.Msg.GetOrgId()); err != nil {
		return nil, err
	}
	if err := s.requireCipher(); err != nil {
		return nil, err
	}

	kind, cfg, err := configFromSetRequest(req.Msg)
	if err != nil {
		return nil, apperr.Invalid(apperr.ReasonInvalidEmailProviderConfig, "invalid email provider config")
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

	// Cache invalidation brackets the upsert: the "before" invalidate guarantees
	// a concurrent reader cannot keep serving the old ciphertext indefinitely,
	// and the "after" invalidate handles a concurrent reader that re-cached the
	// pre-upsert value during the upsert window. Without bracketing, a Redis
	// hiccup after a successful upsert leaves the rotated-out credential
	// serving real email sends for up to orgEmailProviderCacheTTL.
	if s.repo != nil {
		if err := s.repo.Invalidate(ctx, req.Msg.GetOrgId()); err != nil {
			return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
		}
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

	if s.repo != nil {
		if err := s.repo.Invalidate(ctx, req.Msg.GetOrgId()); err != nil {
			return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
		}
	}

	return connect.NewResponse(&orgemailprovidersv1.SetResponse{
		UpdateTime: timestamppb.New(row.UpdateTime.Time),
	}), nil
}

func (s *server) Remove(ctx context.Context, req *connect.Request[orgemailprovidersv1.RemoveRequest]) (*connect.Response[orgemailprovidersv1.RemoveResponse], error) {
	if _, err := s.requireAdmin(ctx, req.Msg.GetOrgId()); err != nil {
		return nil, err
	}
	if _, err := s.write.DeleteOrgEmailProvider(ctx, req.Msg.GetOrgId()); err != nil {
		slog.ErrorContext(ctx, "failed to delete org email provider", slogx.Error(err),
			slog.String("org_id", req.Msg.GetOrgId()))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	// Cleanup of stale rows must work in operator-only mode (repo is nil),
	// even without cache invalidation.
	if s.repo != nil {
		if err := s.repo.Invalidate(ctx, req.Msg.GetOrgId()); err != nil {
			return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
		}
	}
	return connect.NewResponse(&orgemailprovidersv1.RemoveResponse{}), nil
}

// Provider failures land in the response body (not as RPC errors) so the
// admin sees the underlying provider message. The recipient is restricted to
// the calling admin's own email to keep the operator-default reputation
// domain from becoming a free open-relay.
func (s *server) SendTest(ctx context.Context, req *connect.Request[orgemailprovidersv1.SendTestRequest]) (*connect.Response[orgemailprovidersv1.SendTestResponse], error) {
	principal, err := s.requireAdmin(ctx, req.Msg.GetOrgId())
	if err != nil {
		return nil, err
	}
	if s.mailer == nil {
		return nil, apperr.FailedPrecondition(apperr.ReasonEmailTestSendUnavailable,
			"test send is not available on this server")
	}
	if !strings.EqualFold(req.Msg.GetRecipient(), principal.Customer.Email) {
		return nil, apperr.PermissionDenied(apperr.ReasonEmailTestRecipientMismatch,
			"test recipient must match the calling admin's email")
	}

	// Unique-per-call key: an admin who fixes their provider config (auth, host,
	// sender address, etc.) and re-clicks "Send Test" must NOT be silently
	// deduped by Resend's upstream 24h idempotency window. The prefix is
	// decorative for log diagnostics; the xid carries the dedup-defeating
	// entropy. Recipient is constrained to the admin's own email above, so
	// this can't be used to spam arbitrary addresses.
	idempotencyKey := "send_test:" + req.Msg.GetOrgId() + ":" + req.Msg.GetRecipient() + ":" + xid.New().String()
	if err := s.mailer.SendTest(ctx, req.Msg.GetOrgId(), req.Msg.GetRecipient(), idempotencyKey); err != nil {
		// Permanent errors are already logged at source by the resolver layer;
		// render failures and transient provider-send errors are not, so record
		// the non-permanent case here rather than letting it vanish into the
		// response body.
		if !coreemail.IsPermanentError(err) {
			slog.ErrorContext(ctx, "send test email failed",
				slogx.Error(err), slog.String("org_id", req.Msg.GetOrgId()))
			telemetry.RecordError(ctx, err)
		}
		errMsg := classifySendTestError(err)
		return connect.NewResponse(&orgemailprovidersv1.SendTestResponse{
			Success:      boolPtr(false),
			ErrorMessage: &errMsg,
		}), nil
	}
	return connect.NewResponse(&orgemailprovidersv1.SendTestResponse{Success: boolPtr(true)}), nil
}

// classifySendTestError converts a raw resolver/provider error into a
// client-safe message for the SendTest response body — this only sanitises what
// the admin sees in the dashboard. (Logging is handled by the caller: permanent
// resolver errors at their source, non-permanent render/send errors in
// SendTest.) Internal package paths like "secret:" or "email:" are stripped so
// a decrypt failure doesn't leak the cipher package name to the UI.
func classifySendTestError(err error) string {
	if err == nil {
		return ""
	}
	if coreemail.IsPermanentError(err) {
		msg := err.Error()
		if strings.Contains(msg, "decrypt") || strings.Contains(msg, "cipher:") {
			return "Stored secret cannot be decrypted with the server's current encryption key; please re-save the provider config."
		}
		if strings.Contains(msg, "unknown provider kind") {
			return "Provider kind is not supported by this server; please contact your operator."
		}
		return stripInternalPrefixes(msg)
	}
	return stripInternalPrefixes(err.Error())
}

// stripInternalPrefixes removes package-name error wraps like "secret: ",
// "email: ", "smtp: ", "resend send email: ", "ses send email: " from the
// start of a message so the admin doesn't see Go package names. Leaves the
// underlying provider message (e.g. AWS error text) intact.
func stripInternalPrefixes(msg string) string {
	for _, prefix := range []string{"secret: ", "email: ", "smtp: ", "ses send email: ", "resend send email: "} {
		msg = strings.TrimPrefix(msg, prefix)
	}
	return msg
}

func boolPtr(b bool) *bool { return &b }
