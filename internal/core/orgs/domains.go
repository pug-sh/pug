package orgs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/domainname"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
	"github.com/rs/xid"
)

var (
	ErrDomainInvalid         = errors.New("invalid domain")
	ErrDomainNotFound        = errors.New("domain not found")
	ErrDomainLimitReached    = errors.New("org has reached its domain limit")
	ErrDomainNotVerified     = errors.New("org has no verified domain")
	ErrOrgCreationRestricted = errors.New("org creation is restricted for this email domain")
	ErrDNSUnavailable        = errors.New("dns lookup failed")
	ErrDomainSSONotSeen      = errors.New("no one has signed in to the domain through SSO")
)

// SSORequiredError refuses a sign-in that didn't prove Domain, which requires SSO.
type SSORequiredError struct {
	Domain string
	// The refused sign-in was an invite link.
	Invite bool
}

func (e *SSORequiredError) Error() string {
	return e.Domain + " requires SSO"
}

// DomainVerificationError reports the domain with no TXT record holding the org's value.
type DomainVerificationError struct {
	Domain string
}

func (e *DomainVerificationError) Error() string {
	return "no matching verification record for " + e.Domain
}

const (
	maxDomainsPerOrg = 10
	dnsLookupTimeout = 5 * time.Second

	verificationRecordPrefix = "_pug-verification."
	verificationValuePrefix  = "pug-verification="

	VerificationMethodDNS      = "dns"
	VerificationMethodOperator = "operator"
)

// TXTResolver is the slice of *net.Resolver domain verification uses.
type TXTResolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
}

// Domain is one org's claim to a domain.
type Domain struct {
	ID                 string
	Domain             string
	VerificationToken  string
	VerifiedAt         time.Time
	VerificationMethod string
	RequireSSO         bool
	SSOSeen            bool
	// Set only by ListDomains, and only on a verified domain.
	OrgCreationRestrictedElsewhere bool
	SSORequiredElsewhere           bool
}

func (d Domain) Verified() bool { return !d.VerifiedAt.IsZero() }

func (d Domain) TXTRecordName() string { return verificationRecordPrefix + d.Domain }

func (d Domain) TXTRecordValue() string { return verificationValuePrefix + d.VerificationToken }

// DomainSettings are an org's domain settings. An empty AutoJoinRole turns auto-join off.
type DomainSettings struct {
	AutoJoinRole         Role
	MembersCanCreateOrgs bool
}

// WithTXTResolver swaps the DNS resolver domain verification uses.
func (s *Service) WithTXTResolver(r TXTResolver) *Service {
	s.resolver = r
	return s
}

func domainFromRow(r dbwrite.OrgDomain) Domain {
	return Domain{
		ID:                 r.ID,
		Domain:             r.Domain,
		VerificationToken:  r.VerificationToken,
		VerifiedAt:         r.VerifiedAt.Time,
		VerificationMethod: r.VerificationMethod.String,
		RequireSSO:         r.RequireSso,
		SSOSeen:            r.SsoSeenAt.Valid,
	}
}

func settingsFromOrg(autoJoinRole string, membersCanCreateOrgs bool) DomainSettings {
	return DomainSettings{AutoJoinRole: Role(autoJoinRole), MembersCanCreateOrgs: membersCanCreateOrgs}
}

// ListDomains reads the primary so an admin sees the domain they just added.
func (s *Service) ListDomains(ctx context.Context, orgID string) (DomainSettings, []Domain, error) {
	r := dbread.New(s.pgW)
	org, err := r.GetOrgByID(ctx, orgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DomainSettings{}, nil, ErrOrgNotFound
		}
		slog.ErrorContext(ctx, "failed to get org for domains", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return DomainSettings{}, nil, err
	}
	rows, err := r.ListOrgDomainsByOrgID(ctx, orgID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to list org domains", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return DomainSettings{}, nil, err
	}
	domains := make([]Domain, 0, len(rows))
	for _, row := range rows {
		domains = append(domains, Domain{
			ID:                             row.ID,
			Domain:                         row.Domain,
			VerificationToken:              row.VerificationToken,
			VerifiedAt:                     row.VerifiedAt.Time,
			VerificationMethod:             row.VerificationMethod.String,
			RequireSSO:                     row.RequireSso,
			SSOSeen:                        row.SsoSeenAt.Valid,
			OrgCreationRestrictedElsewhere: row.OrgCreationRestrictedElsewhere,
			SSORequiredElsewhere:           row.SsoRequiredElsewhere,
		})
	}
	return settingsFromOrg(org.AutoJoinRole.String, org.MembersCanCreateOrgs), domains, nil
}

// AddDomain adds a pending domain, or returns the org's existing row for it.
func (s *Service) AddDomain(ctx context.Context, orgID, rawDomain string) (Domain, error) {
	domain, err := domainname.Normalize(rawDomain)
	if err != nil {
		return Domain{}, ErrDomainInvalid
	}

	tx, err := s.pgW.Begin(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to begin add domain transaction", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return Domain{}, err
	}
	defer func() {
		if rollbackErr := tx.Rollback(ctx); rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			slog.ErrorContext(ctx, "failed rolling back add domain transaction", slogx.Error(rollbackErr))
			telemetry.RecordError(ctx, rollbackErr)
		}
	}()

	w := dbwrite.New(tx)
	// Locks the org so concurrent adds can't both pass the limit.
	if _, err := w.GetOrgByIDForUpdate(ctx, orgID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Domain{}, ErrOrgNotFound
		}
		slog.ErrorContext(ctx, "failed to lock org for add domain", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return Domain{}, err
	}
	existing, err := w.GetOrgDomainByOrgIDAndDomain(ctx, dbwrite.GetOrgDomainByOrgIDAndDomainParams{OrgID: orgID, Domain: domain})
	if err == nil {
		return domainFromRow(existing), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		slog.ErrorContext(ctx, "failed to look up org domain", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return Domain{}, err
	}
	n, err := w.CountOrgDomainsByOrgID(ctx, orgID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to count org domains", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return Domain{}, err
	}
	if n >= maxDomainsPerOrg {
		return Domain{}, ErrDomainLimitReached
	}
	token, err := newRandomToken()
	if err != nil {
		slog.ErrorContext(ctx, "failed to generate domain verification token", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return Domain{}, err
	}
	row, err := w.CreateOrgDomain(ctx, dbwrite.CreateOrgDomainParams{
		ID:                xid.New().String(),
		OrgID:             orgID,
		Domain:            domain,
		VerificationToken: token,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to create org domain", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return Domain{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "failed to commit add domain transaction", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return Domain{}, err
	}
	return domainFromRow(row), nil
}

// VerifyDomain marks the domain verified if its TXT record is there now.
func (s *Service) VerifyDomain(ctx context.Context, orgID, domainID string) (Domain, error) {
	d, err := s.getDomain(ctx, orgID, domainID)
	if err != nil || d.Verified() {
		return d, err
	}
	if err := s.checkTXT(ctx, d); err != nil {
		return Domain{}, err
	}
	row, err := s.write.MarkOrgDomainVerifiedByDNS(ctx, dbwrite.MarkOrgDomainVerifiedByDNSParams{ID: domainID, OrgID: orgID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Verified or removed by a concurrent call; report whichever it was.
			return s.getDomain(ctx, orgID, domainID)
		}
		slog.ErrorContext(ctx, "failed to mark org domain verified", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return Domain{}, err
	}
	return domainFromRow(row), nil
}

// RemoveDomain drops this org's claim. Members who joined through it stay.
func (s *Service) RemoveDomain(ctx context.Context, orgID, domainID string) error {
	n, err := s.write.DeleteOrgDomain(ctx, dbwrite.DeleteOrgDomainParams{ID: domainID, OrgID: orgID})
	if err != nil {
		slog.ErrorContext(ctx, "failed to delete org domain", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return err
	}
	if n == 0 {
		return ErrDomainNotFound
	}
	return nil
}

// SetDomainSettings replaces both settings. A change that grants or limits more
// re-checks the org's DNS records first.
func (s *Service) SetDomainSettings(ctx context.Context, orgID string, want DomainSettings) (DomainSettings, error) {
	cur, domains, err := s.ListDomains(ctx, orgID)
	if err != nil {
		return DomainSettings{}, err
	}
	grantsMore := (want.AutoJoinRole == RoleMember && cur.AutoJoinRole != RoleMember) ||
		(want.AutoJoinRole == RoleViewer && cur.AutoJoinRole == "")
	limitsMore := !want.MembersCanCreateOrgs && cur.MembersCanCreateOrgs
	if grantsMore || limitsMore {
		if err := s.recheckDomains(ctx, domains); err != nil {
			return DomainSettings{}, err
		}
	}
	org, err := s.write.UpdateOrgDomainSettings(ctx, dbwrite.UpdateOrgDomainSettingsParams{
		ID:                   orgID,
		AutoJoinRole:         postgres.NewOptionalText(want.AutoJoinRole.String()),
		MembersCanCreateOrgs: want.MembersCanCreateOrgs,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DomainSettings{}, ErrOrgNotFound
		}
		slog.ErrorContext(ctx, "failed to update org domain settings", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return DomainSettings{}, err
	}
	return settingsFromOrg(org.AutoJoinRole.String, org.MembersCanCreateOrgs), nil
}

// UpdateDomain turns Require SSO on or off for the org's claim. Turning it on needs a
// verified domain that an SSO sign-in has proven, and re-checks its DNS record.
func (s *Service) UpdateDomain(ctx context.Context, orgID, domainID string, requireSSO bool) (Domain, error) {
	d, err := s.getDomain(ctx, orgID, domainID)
	if err != nil {
		return Domain{}, err
	}
	if requireSSO && !d.RequireSSO {
		switch {
		case !d.Verified():
			return Domain{}, ErrDomainNotVerified
		case !d.SSOSeen:
			return Domain{}, ErrDomainSSONotSeen
		case d.VerificationMethod != VerificationMethodOperator:
			if err := s.checkTXT(ctx, d); err != nil {
				return Domain{}, err
			}
		}
	}
	row, err := s.write.UpdateOrgDomainRequireSSO(ctx, dbwrite.UpdateOrgDomainRequireSSOParams{
		ID:         domainID,
		OrgID:      orgID,
		RequireSso: requireSSO,
	})
	if err != nil {
		// verified_at and sso_seen_at are never unset, so the claim was removed meanwhile.
		if errors.Is(err, pgx.ErrNoRows) {
			return Domain{}, ErrDomainNotFound
		}
		slog.ErrorContext(ctx, "failed to update org domain", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return Domain{}, err
	}
	return domainFromRow(row), nil
}

// recheckDomains needs a verified domain, and a live record for each DNS-verified one.
func (s *Service) recheckDomains(ctx context.Context, domains []Domain) error {
	verified := false
	for _, d := range domains {
		if !d.Verified() {
			continue
		}
		verified = true
		if d.VerificationMethod == VerificationMethodOperator {
			continue
		}
		if err := s.checkTXT(ctx, d); err != nil {
			return err
		}
	}
	if !verified {
		return ErrDomainNotVerified
	}
	return nil
}

func (s *Service) checkTXT(ctx context.Context, d Domain) error {
	ctx, cancel := context.WithTimeout(ctx, dnsLookupTimeout)
	defer cancel()
	// Rooted, so the resolver doesn't try the pod's search domains first.
	records, err := s.resolver.LookupTXT(ctx, d.TXTRecordName()+".")
	if err != nil {
		if dnsErr, ok := errors.AsType[*net.DNSError](err); ok && dnsErr.IsNotFound {
			return &DomainVerificationError{Domain: d.Domain}
		}
		slog.WarnContext(ctx, "domain verification lookup failed", slogx.Error(err), slog.String("domain", d.Domain))
		return ErrDNSUnavailable
	}
	want := d.TXTRecordValue()
	for _, record := range records {
		if strings.TrimSpace(record) == want {
			return nil
		}
	}
	return &DomainVerificationError{Domain: d.Domain}
}

func (s *Service) getDomain(ctx context.Context, orgID, domainID string) (Domain, error) {
	row, err := s.write.GetOrgDomainByID(ctx, dbwrite.GetOrgDomainByIDParams{ID: domainID, OrgID: orgID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Domain{}, ErrDomainNotFound
		}
		slog.ErrorContext(ctx, "failed to get org domain", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return Domain{}, err
	}
	return domainFromRow(row), nil
}

// VerifyDomainByOperator verifies the domain without DNS, adding it if needed.
func (s *Service) VerifyDomainByOperator(ctx context.Context, orgID, rawDomain string) (Domain, error) {
	domain, err := domainname.Normalize(rawDomain)
	if err != nil {
		return Domain{}, ErrDomainInvalid
	}
	if _, err := s.write.GetOrgByID(ctx, orgID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Domain{}, ErrOrgNotFound
		}
		return Domain{}, fmt.Errorf("get org: %w", err)
	}
	token, err := newRandomToken()
	if err != nil {
		return Domain{}, err
	}
	row, err := s.write.UpsertOrgDomainVerifiedByOperator(ctx, dbwrite.UpsertOrgDomainVerifiedByOperatorParams{
		ID:                xid.New().String(),
		OrgID:             orgID,
		Domain:            domain,
		VerificationToken: token,
	})
	if err != nil {
		return Domain{}, fmt.Errorf("verify domain: %w", err)
	}
	return domainFromRow(row), nil
}

// ReleaseDomain drops one org's claim to a domain, verified or not.
func (s *Service) ReleaseDomain(ctx context.Context, orgID, rawDomain string) error {
	domain, err := domainname.Normalize(rawDomain)
	if err != nil {
		return ErrDomainInvalid
	}
	// So a mistyped org id doesn't read as "this org has no claim".
	if _, err := s.write.GetOrgByID(ctx, orgID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrOrgNotFound
		}
		return fmt.Errorf("get org: %w", err)
	}
	n, err := s.write.DeleteOrgDomainByOrgIDAndDomain(ctx, dbwrite.DeleteOrgDomainByOrgIDAndDomainParams{OrgID: orgID, Domain: domain})
	if err != nil {
		return fmt.Errorf("release domain: %w", err)
	}
	if n == 0 {
		return ErrDomainNotFound
	}
	return nil
}

// UnenforceDomain turns Require SSO off in every org that claimed the domain, for when
// SSO breaks and nobody on the domain can sign in to do it. It returns how many claims changed.
func (s *Service) UnenforceDomain(ctx context.Context, rawDomain string) (int64, error) {
	domain, err := domainname.Normalize(rawDomain)
	if err != nil {
		return 0, ErrDomainInvalid
	}
	n, err := s.write.UnenforceOrgDomainRequireSSO(ctx, domain)
	if err != nil {
		return 0, fmt.Errorf("unenforce domain: %w", err)
	}
	// So a mistyped domain doesn't read as "nothing to turn off".
	if n == 0 {
		claims, err := s.DomainClaims(ctx, domain)
		if err != nil {
			return 0, err
		}
		if len(claims) == 0 {
			return 0, ErrDomainNotFound
		}
	}
	return n, nil
}

// DomainClaims lists every org that has added the domain, verified ones first.
func (s *Service) DomainClaims(ctx context.Context, rawDomain string) ([]dbread.ListOrgDomainsByDomainRow, error) {
	domain, err := domainname.Normalize(rawDomain)
	if err != nil {
		return nil, ErrDomainInvalid
	}
	return dbread.New(s.pgW).ListOrgDomainsByDomain(ctx, domain)
}

// OrgCreationAllowed only hides the button; CreateOrgWithDefaults enforces the rule.
func (s *Service) OrgCreationAllowed(ctx context.Context, customerID, email string) (bool, error) {
	return OrgCreationAllowedInTx(ctx, s.write, customerID, email)
}

// AutoJoinInTx adds the customer to every org that verified provenDomain with auto-join
// on, skipping memberships and pending invites. It returns the orgs joined.
func AutoJoinInTx(ctx context.Context, w *dbwrite.Queries, customerID, email, provenDomain string) ([]string, error) {
	if provenDomain == "" {
		return nil, nil
	}
	joined, err := w.AutoJoinOrgsByDomain(ctx, dbwrite.AutoJoinOrgsByDomainParams{
		CustomerID: customerID,
		Domain:     provenDomain,
		Email:      email,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to auto-join orgs", slogx.Error(err),
			slog.String("customer_id", customerID), slog.String("domain", provenDomain))
		telemetry.RecordError(ctx, err)
		return nil, err
	}
	return joined, nil
}

// MarkSSOSeenInTx marks every verified claim on provenDomain as proven by an SSO sign-in.
func MarkSSOSeenInTx(ctx context.Context, w *dbwrite.Queries, provenDomain string) error {
	if provenDomain == "" {
		return nil
	}
	if err := w.MarkOrgDomainsSSOSeen(ctx, provenDomain); err != nil {
		slog.ErrorContext(ctx, "failed to mark domain sso seen", slogx.Error(err), slog.String("domain", provenDomain))
		telemetry.RecordError(ctx, err)
		return err
	}
	return nil
}

// CheckSignInInTx refuses a sign-in for an email whose domain requires SSO, unless the
// sign-in proved that domain. It returns *SSORequiredError.
func CheckSignInInTx(ctx context.Context, w *dbwrite.Queries, email, provenDomain string) error {
	domain := domainname.Of(email)
	if domain == "" || domain == provenDomain {
		return nil
	}
	required, err := w.IsSSORequired(ctx, domain)
	if err != nil {
		slog.ErrorContext(ctx, "failed to check whether the domain requires sso", slogx.Error(err), slog.String("domain", domain))
		telemetry.RecordError(ctx, err)
		return err
	}
	if required {
		return &SSORequiredError{Domain: domain}
	}
	return nil
}

// OrgCreationAllowedInTx is false when any org that verified the email's domain
// turned org creation off, unless the customer is an admin of one of them.
func OrgCreationAllowedInTx(ctx context.Context, w *dbwrite.Queries, customerID, email string) (bool, error) {
	domain := domainname.Of(email)
	if domain == "" {
		return true, nil
	}
	restricted, err := w.IsOrgCreationRestricted(ctx, dbwrite.IsOrgCreationRestrictedParams{
		Domain:     domain,
		CustomerID: customerID,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to check org creation restriction", slogx.Error(err), slog.String("customer_id", customerID))
		telemetry.RecordError(ctx, err)
		return false, err
	}
	return !restricted, nil
}
