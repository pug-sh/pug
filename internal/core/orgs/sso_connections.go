package orgs

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"code.dny.dev/ssrf"
	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5"
	appconfig "github.com/pug-sh/pug/internal/config"
	coreoauth "github.com/pug-sh/pug/internal/core/auth/oauth"
	"github.com/pug-sh/pug/internal/core/email/secret"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
	"github.com/rs/xid"
)

var (
	ErrSSOConnectionsDisabled      = errors.New("sso connections are off: no secret key")
	ErrSSOConnectionNotFound       = errors.New("sso connection not found")
	ErrSSOConnectionLimitReached   = errors.New("org has reached its sso connection limit")
	ErrSSOConnectionIssuerInvalid  = errors.New("invalid sso connection issuer")
	ErrSSOConnectionGoogleIssuer   = errors.New("sso connection can't use google's issuer")
	ErrSSOConnectionDiscovery      = errors.New("sso connection issuer discovery failed")
	ErrSSOConnectionIssuerMismatch = errors.New("sso connection issuer differs from its discovery document's")
	ErrSSOConnectionPrivateIssuer  = errors.New("sso connection issuer is on a private address")
	ErrSSOConnectionSecretRequired = errors.New("sso connection needs its client secret again")
	ErrDomainHasSSOConnection      = errors.New("another sso connection signs in the domain")
)

const (
	maxSSOConnectionsPerOrg = 10
	discoveryTimeout        = 10 * time.Second

	ssoConnectionDomainKey = "org_domains_sso_connection_domain_key"
)

type IssuerChecker func(ctx context.Context, issuerURL string) error

// SSOConnection is an org's OIDC connection. Its secret never leaves the server.
type SSOConnection struct {
	ID        string
	Label     string
	IssuerURL string
	ClientID  string
	Domains   []SSOConnectionDomain
}

type SSOConnectionDomain struct {
	ID     string
	Domain string
}

// SSOConnectionInput creates a connection when ID is empty. An empty ClientSecret keeps the
// stored one, unless the issuer or client id changes or the key can't read it.
type SSOConnectionInput struct {
	ID           string
	Label        string
	IssuerURL    string
	ClientID     string
	ClientSecret string
	DomainIDs    []string
}

// WithSSOConnections turns on SSO connections. Without it, their RPCs fail with ErrSSOConnectionsDisabled.
func (s *Service) WithSSOConnections(cipher *secret.Cipher, checkIssuer IssuerChecker) *Service {
	s.ssoCipher = cipher
	s.checkIssuer = checkIssuer
	return s
}

// ListSSOConnections reads the primary so an admin sees the connection they just saved.
func (s *Service) ListSSOConnections(ctx context.Context, orgID string) ([]SSOConnection, error) {
	if s.ssoCipher == nil {
		return nil, ErrSSOConnectionsDisabled
	}
	rows, err := s.write.ListSSOConnectionsByOrgID(ctx, orgID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to list sso connections", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return nil, err
	}
	domains, err := s.write.ListOrgDomainsWithSSOConnection(ctx, orgID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to list sso connection domains", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return nil, err
	}
	out := make([]SSOConnection, 0, len(rows))
	for _, row := range rows {
		c := connectionFromRow(row)
		for _, d := range domains {
			if d.SsoConnectionID.String == row.ID {
				c.Domains = append(c.Domains, SSOConnectionDomain{ID: d.ID, Domain: d.Domain})
			}
		}
		out = append(out, c)
	}
	return out, nil
}

// SetSSOConnection creates or updates a connection and the domains it signs in. New domains or
// a new issuer re-check the TXT records. A new issuer or client id needs the secret again, and a
// new issuer drops linked identities, since a sub is unique only within one issuer.
func (s *Service) SetSSOConnection(ctx context.Context, orgID string, in SSOConnectionInput) (SSOConnection, error) {
	if s.ssoCipher == nil {
		return SSOConnection{}, ErrSSOConnectionsDisabled
	}
	issuer, err := appconfig.ValidateIssuer(in.IssuerURL)
	if err != nil {
		return SSOConnection{}, ErrSSOConnectionIssuerInvalid
	}
	// Google proves domains through hd. A connection would also accept personal accounts.
	if appconfig.IsGoogleIssuer(issuer) {
		return SSOConnection{}, ErrSSOConnectionGoogleIssuer
	}
	domainIDs := slices.Compact(slices.Sorted(slices.Values(in.DomainIDs)))

	// Checked before the transaction, so DNS and discovery hold no lock or connection.
	var cur dbwrite.OrgSsoConnection
	var curDomains []string
	if in.ID != "" {
		if cur, err = getSSOConnection(ctx, s.write, orgID, in.ID); err != nil {
			return SSOConnection{}, err
		}
		if curDomains, err = s.write.ListSSOConnectionDomains(ctx, in.ID); err != nil {
			slog.ErrorContext(ctx, "failed to list sso connection domains", slogx.Error(err), slog.String("org_id", orgID))
			telemetry.RecordError(ctx, err)
			return SSOConnection{}, err
		}
	}
	// The stored secret belongs to the old issuer and client.
	if (cur.IssuerUrl != issuer || cur.ClientID != in.ClientID) && in.ClientSecret == "" {
		return SSOConnection{}, ErrSSOConnectionSecretRequired
	}
	rows, err := s.write.ListOrgDomainsByIDs(ctx, dbwrite.ListOrgDomainsByIDsParams{OrgID: orgID, Ids: domainIDs})
	if err != nil {
		slog.ErrorContext(ctx, "failed to get org domains for sso connection", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return SSOConnection{}, err
	}
	if len(rows) != len(domainIDs) {
		return SSOConnection{}, ErrDomainNotFound
	}
	var domains []string
	for _, row := range rows {
		d := domainFromRow(row)
		if !d.Verified() {
			return SSOConnection{}, ErrDomainNotVerified
		}
		recheck := cur.IssuerUrl != issuer || !slices.Contains(curDomains, d.Domain)
		if recheck && d.VerificationMethod != VerificationMethodOperator {
			if err := s.checkTXT(ctx, d); err != nil {
				return SSOConnection{}, err
			}
		}
		domains = append(domains, d.Domain)
	}

	discoverCtx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	err = s.checkIssuer(discoverCtx, issuer)
	cancel()
	if err != nil {
		// The body of a failed fetch is the issuer's, so only its start is logged.
		slog.WarnContext(ctx, "sso connection issuer discovery failed", slogx.Error(fmt.Errorf("%.2048s", err.Error())), slog.String("org_id", orgID))
		// Two causes an admin can't see in the logs.
		if _, ok := errors.AsType[*oidc.IssuerMismatchError](err); ok {
			return SSOConnection{}, ErrSSOConnectionIssuerMismatch
		}
		if errors.Is(err, ssrf.ErrProhibitedIP) {
			return SSOConnection{}, ErrSSOConnectionPrivateIssuer
		}
		return SSOConnection{}, ErrSSOConnectionDiscovery
	}

	tx, err := s.pgW.Begin(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to begin sso connection transaction", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return SSOConnection{}, err
	}
	defer rollback(ctx, tx, "sso connection")
	w := dbwrite.New(tx)

	if in.ID != "" {
		// Read again under the lock: it decides whether the stored secret can stay and
		// whether linked identities go.
		if cur, err = getSSOConnection(ctx, w, orgID, in.ID); err != nil {
			return SSOConnection{}, err
		}
		if (cur.IssuerUrl != issuer || cur.ClientID != in.ClientID) && in.ClientSecret == "" {
			return SSOConnection{}, ErrSSOConnectionSecretRequired
		}
	} else {
		// Locks the org so concurrent creates can't both pass the limit. Only creates: a
		// sign-in locks the connection, then the org when it auto-joins.
		if _, err := w.GetOrgByIDForUpdate(ctx, orgID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return SSOConnection{}, ErrOrgNotFound
			}
			slog.ErrorContext(ctx, "failed to lock org for sso connection", slogx.Error(err), slog.String("org_id", orgID))
			telemetry.RecordError(ctx, err)
			return SSOConnection{}, err
		}
		n, err := w.CountSSOConnectionsByOrgID(ctx, orgID)
		if err != nil {
			slog.ErrorContext(ctx, "failed to count sso connections", slogx.Error(err), slog.String("org_id", orgID))
			telemetry.RecordError(ctx, err)
			return SSOConnection{}, err
		}
		if n >= maxSSOConnectionsPerOrg {
			return SSOConnection{}, ErrSSOConnectionLimitReached
		}
	}

	ciphertext := cur.ClientSecretCiphertext
	if in.ClientSecret != "" {
		if ciphertext, err = s.ssoCipher.Encrypt([]byte(in.ClientSecret)); err != nil {
			slog.ErrorContext(ctx, "failed to encrypt sso connection secret", slogx.Error(err))
			telemetry.RecordError(ctx, err)
			return SSOConnection{}, err
		}
	} else if _, err := s.ssoCipher.Decrypt(ciphertext); err != nil {
		// Saved under another PUG_SSO_SECRET_KEY.
		return SSOConnection{}, ErrSSOConnectionSecretRequired
	}

	var row dbwrite.OrgSsoConnection
	if in.ID == "" {
		row, err = w.CreateSSOConnection(ctx, dbwrite.CreateSSOConnectionParams{
			ID:                     xid.New().String(),
			OrgID:                  orgID,
			Label:                  in.Label,
			IssuerUrl:              issuer,
			ClientID:               in.ClientID,
			ClientSecretCiphertext: ciphertext,
		})
	} else {
		row, err = w.UpdateSSOConnection(ctx, dbwrite.UpdateSSOConnectionParams{
			ID:                     in.ID,
			OrgID:                  orgID,
			Label:                  in.Label,
			IssuerUrl:              issuer,
			ClientID:               in.ClientID,
			ClientSecretCiphertext: ciphertext,
		})
	}
	if err != nil {
		slog.ErrorContext(ctx, "failed to save sso connection", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return SSOConnection{}, err
	}
	if in.ID != "" && cur.IssuerUrl != issuer {
		if err := deleteConnectionIdentitiesInTx(ctx, w, row.ID); err != nil {
			return SSOConnection{}, err
		}
		// Their SSO sign-ins went through the old issuer.
		if err := clearSSOSeenInTx(ctx, w, domains); err != nil {
			return SSOConnection{}, err
		}
	}

	detached, err := w.DetachOrgDomainsFromSSOConnection(ctx, dbwrite.DetachOrgDomainsFromSSOConnectionParams{
		OrgID:           orgID,
		SsoConnectionID: row.ID,
		KeepIds:         domainIDs,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to detach sso connection domains", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return SSOConnection{}, err
	}
	if err := dropDomainsInTx(ctx, w, detached); err != nil {
		return SSOConnection{}, err
	}
	attached, err := w.AttachOrgDomainsToSSOConnection(ctx, dbwrite.AttachOrgDomainsToSSOConnectionParams{
		OrgID:           orgID,
		SsoConnectionID: row.ID,
		Ids:             domainIDs,
	})
	if err != nil {
		if isUniqueViolationOn(err, ssoConnectionDomainKey) {
			return SSOConnection{}, ErrDomainHasSSOConnection
		}
		slog.ErrorContext(ctx, "failed to attach sso connection domains", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return SSOConnection{}, err
	}
	// Another of the org's connections signs in one of them.
	if attached != int64(len(domainIDs)) {
		return SSOConnection{}, ErrDomainHasSSOConnection
	}

	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "failed to commit sso connection transaction", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return SSOConnection{}, err
	}
	out := connectionFromRow(row)
	for _, r := range rows {
		out.Domains = append(out.Domains, SSOConnectionDomain{ID: r.ID, Domain: r.Domain})
	}
	slices.SortFunc(out.Domains, func(a, b SSOConnectionDomain) int { return cmp.Compare(a.Domain, b.Domain) })
	return out, nil
}

// DeleteSSOConnection deletes the connection and its linked identities. It is refused
// while any org requires SSO for one of its domains.
func (s *Service) DeleteSSOConnection(ctx context.Context, orgID, id string) error {
	if s.ssoCipher == nil {
		return ErrSSOConnectionsDisabled
	}
	tx, err := s.pgW.Begin(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to begin delete sso connection transaction", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return err
	}
	defer rollback(ctx, tx, "delete sso connection")
	w := dbwrite.New(tx)

	if _, err := getSSOConnection(ctx, w, orgID, id); err != nil {
		return err
	}
	domains, err := w.ListSSOConnectionDomains(ctx, id)
	if err != nil {
		slog.ErrorContext(ctx, "failed to list sso connection domains", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return err
	}
	if err := deleteConnectionIdentitiesInTx(ctx, w, id); err != nil {
		return err
	}
	if _, err := w.DeleteSSOConnection(ctx, dbwrite.DeleteSSOConnectionParams{ID: id, OrgID: orgID}); err != nil {
		slog.ErrorContext(ctx, "failed to delete sso connection", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return err
	}
	if err := dropDomainsInTx(ctx, w, domains); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "failed to commit delete sso connection transaction", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return err
	}
	return nil
}

func SSOConnectionForSignIn(ctx context.Context, w *dbwrite.Queries, cipher *secret.Cipher, id string) (coreoauth.Connection, error) {
	row, err := w.GetSSOConnectionByID(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return coreoauth.Connection{}, ErrSSOConnectionNotFound
		}
		slog.ErrorContext(ctx, "failed to get sso connection for sign-in", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return coreoauth.Connection{}, err
	}
	domains, err := w.ListSSOConnectionDomains(ctx, id)
	if err != nil {
		slog.ErrorContext(ctx, "failed to list sso connection domains for sign-in", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return coreoauth.Connection{}, err
	}
	plain, err := cipher.Decrypt(row.ClientSecretCiphertext)
	if err != nil {
		err = fmt.Errorf("decrypt sso connection %s secret: %w", id, err)
		slog.ErrorContext(ctx, "failed to decrypt sso connection secret", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return coreoauth.Connection{}, err
	}
	return coreoauth.Connection{
		ID:           row.ID,
		Label:        row.Label,
		IssuerURL:    row.IssuerUrl,
		ClientID:     row.ClientID,
		ClientSecret: string(plain),
		Domains:      domains,
	}, nil
}

// SSOConnectionUnchangedInTx refuses a sign-in whose connection was deleted, or got a new issuer
// or domains, since the sign-in read it, so it can't relink an unlinked sub or prove a dropped
// domain. The share lock waits for a change in flight, except `pug domains release`.
func SSOConnectionUnchangedInTx(ctx context.Context, w *dbwrite.Queries, conn coreoauth.Connection) error {
	issuer, err := w.GetSSOConnectionIssuerForShare(ctx, conn.ID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		slog.ErrorContext(ctx, "failed to recheck sso connection", slogx.Error(err), slog.String("connection_id", conn.ID))
		telemetry.RecordError(ctx, err)
		return err
	}
	domains, err := w.ListSSOConnectionDomains(ctx, conn.ID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to recheck sso connection domains", slogx.Error(err), slog.String("connection_id", conn.ID))
		telemetry.RecordError(ctx, err)
		return err
	}
	if issuer != conn.IssuerURL || !slices.Equal(domains, conn.Domains) {
		slog.WarnContext(ctx, "sso connection changed during sign-in", slog.String("connection_id", conn.ID))
		return coreoauth.ErrOAuthProviderDisabled
	}
	return nil
}

func SSOConnectionForDomain(ctx context.Context, w *dbwrite.Queries, domain string) (SSOConnection, bool, error) {
	row, err := w.GetSSOConnectionByDomain(ctx, domain)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return SSOConnection{}, false, nil
		}
		slog.ErrorContext(ctx, "failed to get sso connection for domain", slogx.Error(err), slog.String("domain", domain))
		telemetry.RecordError(ctx, err)
		return SSOConnection{}, false, err
	}
	return connectionFromRow(row), true, nil
}

// dropDomainsInTx is for domains a connection stops signing in: it clears sso_seen_at, then
// refuses if any org requires SSO for one. The clear locks every org's claim first, so a
// concurrent Require SSO is either seen here or refused for want of a new SSO sign-in.
func dropDomainsInTx(ctx context.Context, w *dbwrite.Queries, domains []string) error {
	if err := clearSSOSeenInTx(ctx, w, domains); err != nil {
		return err
	}
	for _, domain := range domains {
		required, err := w.IsSSORequired(ctx, domain)
		if err != nil {
			slog.ErrorContext(ctx, "failed to check whether the domain requires sso", slogx.Error(err), slog.String("domain", domain))
			telemetry.RecordError(ctx, err)
			return err
		}
		if required {
			return &SSOConnectionInUseError{Domain: domain}
		}
	}
	return nil
}

type SSOConnectionInUseError struct {
	Domain string
}

func (e *SSOConnectionInUseError) Error() string {
	return e.Domain + " requires sso through this connection"
}

func getSSOConnection(ctx context.Context, w *dbwrite.Queries, orgID, id string) (dbwrite.OrgSsoConnection, error) {
	row, err := w.GetSSOConnectionByIDForUpdate(ctx, dbwrite.GetSSOConnectionByIDForUpdateParams{ID: id, OrgID: orgID})
	if errors.Is(err, pgx.ErrNoRows) {
		return row, ErrSSOConnectionNotFound
	}
	if err != nil {
		slog.ErrorContext(ctx, "failed to get sso connection", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
	}
	return row, err
}

// clearSSOSeenInTx makes Require SSO wait for a new SSO sign-in on domains a connection stops
// signing in, or moves to a new issuer, so it can't be turned on with nothing that works.
func clearSSOSeenInTx(ctx context.Context, w *dbwrite.Queries, domains []string) error {
	if len(domains) == 0 {
		return nil
	}
	if err := w.ClearOrgDomainsSSOSeen(ctx, domains); err != nil {
		slog.ErrorContext(ctx, "failed to clear domain sso seen", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return err
	}
	return nil
}

func deleteConnectionIdentitiesInTx(ctx context.Context, w *dbwrite.Queries, id string) error {
	if err := w.DeleteSSOConnectionIdentities(ctx, string(coreoauth.ConnectionProviderName(id))); err != nil {
		slog.ErrorContext(ctx, "failed to delete sso connection identities", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return err
	}
	return nil
}

func connectionFromRow(r dbwrite.OrgSsoConnection) SSOConnection {
	return SSOConnection{ID: r.ID, Label: r.Label, IssuerURL: r.IssuerUrl, ClientID: r.ClientID}
}

func rollback(ctx context.Context, tx pgx.Tx, what string) {
	if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		slog.ErrorContext(ctx, "failed rolling back "+what+" transaction", slogx.Error(err))
		telemetry.RecordError(ctx, err)
	}
}
