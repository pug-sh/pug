package orgs

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	"github.com/pug-sh/pug/internal/apperr"
	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	orgsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/orgs/v1"
)

func (s *server) ListDomains(
	ctx context.Context,
	req *connect.Request[orgsv1.ListDomainsRequest],
) (*connect.Response[orgsv1.ListDomainsResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	settings, domains, err := s.service.ListDomains(ctx, req.Msg.GetOrgId())
	if err != nil {
		return nil, domainError(err, req.Msg.GetOrgId())
	}
	result := make([]*orgsv1.OrgDomain, 0, len(domains))
	for _, d := range domains {
		result = append(result, toRPCDomain(d))
	}
	return connect.NewResponse(&orgsv1.ListDomainsResponse{
		Settings: toRPCDomainSettings(settings),
		Domains:  result,
	}), nil
}

func (s *server) SetDomainSettings(
	ctx context.Context,
	req *connect.Request[orgsv1.SetDomainSettingsRequest],
) (*connect.Response[orgsv1.SetDomainSettingsResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var role coreorgs.Role
	if req.Msg.GetAutoJoinRole() != orgsv1.OrgRole_ORG_ROLE_UNSPECIFIED {
		r, ok := roleFromProto(req.Msg.GetAutoJoinRole())
		if !ok {
			return nil, apperr.Invalid(apperr.ReasonOrgUnsupportedRole, "role enum value not supported by this server")
		}
		role = r
	}

	settings, err := s.service.SetDomainSettings(ctx, req.Msg.GetOrgId(), coreorgs.DomainSettings{
		AutoJoinRole:         role,
		MembersCanCreateOrgs: req.Msg.GetMembersCanCreateOrgs(),
	})
	if err != nil {
		return nil, domainError(err, req.Msg.GetOrgId())
	}
	return connect.NewResponse(&orgsv1.SetDomainSettingsResponse{Settings: toRPCDomainSettings(settings)}), nil
}

func (s *server) AddDomain(
	ctx context.Context,
	req *connect.Request[orgsv1.AddDomainRequest],
) (*connect.Response[orgsv1.AddDomainResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	d, err := s.service.AddDomain(ctx, req.Msg.GetOrgId(), req.Msg.GetDomain())
	if err != nil {
		return nil, domainError(err, req.Msg.GetOrgId())
	}
	return connect.NewResponse(&orgsv1.AddDomainResponse{Domain: toRPCDomain(d)}), nil
}

func (s *server) VerifyDomain(
	ctx context.Context,
	req *connect.Request[orgsv1.VerifyDomainRequest],
) (*connect.Response[orgsv1.VerifyDomainResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	d, err := s.service.VerifyDomain(ctx, req.Msg.GetOrgId(), req.Msg.GetDomainId())
	if err != nil {
		return nil, domainError(err, req.Msg.GetOrgId())
	}
	return connect.NewResponse(&orgsv1.VerifyDomainResponse{Domain: toRPCDomain(d)}), nil
}

func (s *server) RemoveDomain(
	ctx context.Context,
	req *connect.Request[orgsv1.RemoveDomainRequest],
) (*connect.Response[orgsv1.RemoveDomainResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if err := s.service.RemoveDomain(ctx, req.Msg.GetOrgId(), req.Msg.GetDomainId()); err != nil {
		return nil, domainError(err, req.Msg.GetOrgId())
	}
	return connect.NewResponse(&orgsv1.RemoveDomainResponse{}), nil
}

// domainError maps domain errors to RPC errors. Other errors were recorded at source.
func domainError(err error, orgID string) error {
	if verr, ok := errors.AsType[*coreorgs.DomainVerificationError](err); ok {
		return apperr.FailedPrecondition(apperr.ReasonDomainVerificationFailed,
			"no TXT record for "+verr.Domain+" holds this org's verification value; check it and try again",
			apperr.Precondition("DOMAIN_VERIFICATION", verr.Domain, "TXT record missing or doesn't match"))
	}
	switch {
	case errors.Is(err, coreorgs.ErrOrgNotFound):
		return apperr.NotFound(apperr.ReasonOrgNotFound, "org not found", apperr.Resource("org", orgID))
	case errors.Is(err, coreorgs.ErrDomainNotFound):
		return apperr.NotFound(apperr.ReasonDomainNotFound, "domain not found")
	case errors.Is(err, coreorgs.ErrDomainInvalid):
		return apperr.Invalid(apperr.ReasonDomainInvalid, "enter a domain name such as acme.com")
	case errors.Is(err, coreorgs.ErrDomainLimitReached):
		return apperr.FailedPrecondition(apperr.ReasonDomainLimitReached, "an org can add at most 10 domains")
	case errors.Is(err, coreorgs.ErrDomainNotVerified):
		return apperr.FailedPrecondition(apperr.ReasonDomainNotVerified, "verify a domain before turning this on")
	case errors.Is(err, coreorgs.ErrDNSUnavailable):
		return apperr.Unavailable(apperr.ReasonDomainLookupFailed, "we couldn't reach DNS to check the record; try again in a minute")
	default:
		return connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
}
