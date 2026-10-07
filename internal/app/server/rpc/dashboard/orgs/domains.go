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

func (s *server) UpdateDomain(
	ctx context.Context,
	req *connect.Request[orgsv1.UpdateDomainRequest],
) (*connect.Response[orgsv1.UpdateDomainResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	d, err := s.service.UpdateDomain(ctx, req.Msg.GetOrgId(), req.Msg.GetDomainId(), req.Msg.GetRequireSso())
	if err != nil {
		return nil, domainError(err, req.Msg.GetOrgId())
	}
	return connect.NewResponse(&orgsv1.UpdateDomainResponse{Domain: toRPCDomain(d)}), nil
}

func (s *server) ListSSOConnections(
	ctx context.Context,
	req *connect.Request[orgsv1.ListSSOConnectionsRequest],
) (*connect.Response[orgsv1.ListSSOConnectionsResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	conns, err := s.service.ListSSOConnections(ctx, req.Msg.GetOrgId())
	if err != nil {
		return nil, domainError(err, req.Msg.GetOrgId())
	}
	result := make([]*orgsv1.SSOConnection, 0, len(conns))
	for _, c := range conns {
		result = append(result, toRPCSSOConnection(c))
	}
	return connect.NewResponse(&orgsv1.ListSSOConnectionsResponse{Connections: result}), nil
}

func (s *server) SetSSOConnection(
	ctx context.Context,
	req *connect.Request[orgsv1.SetSSOConnectionRequest],
) (*connect.Response[orgsv1.SetSSOConnectionResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	c, err := s.service.SetSSOConnection(ctx, req.Msg.GetOrgId(), coreorgs.SSOConnectionInput{
		ID:           req.Msg.GetConnectionId(),
		Label:        req.Msg.GetLabel(),
		IssuerURL:    req.Msg.GetIssuerUrl(),
		ClientID:     req.Msg.GetClientId(),
		ClientSecret: req.Msg.GetClientSecret(),
		DomainIDs:    req.Msg.GetDomainIds(),
	})
	if err != nil {
		return nil, domainError(err, req.Msg.GetOrgId())
	}
	return connect.NewResponse(&orgsv1.SetSSOConnectionResponse{Connection: toRPCSSOConnection(c)}), nil
}

func (s *server) DeleteSSOConnection(
	ctx context.Context,
	req *connect.Request[orgsv1.DeleteSSOConnectionRequest],
) (*connect.Response[orgsv1.DeleteSSOConnectionResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if err := s.service.DeleteSSOConnection(ctx, req.Msg.GetOrgId(), req.Msg.GetConnectionId()); err != nil {
		return nil, domainError(err, req.Msg.GetOrgId())
	}
	return connect.NewResponse(&orgsv1.DeleteSSOConnectionResponse{}), nil
}

// domainError maps domain and SSO connection errors to RPC errors. Other errors were recorded at source.
func domainError(err error, orgID string) error {
	if inUse, ok := errors.AsType[*coreorgs.SSOConnectionInUseError](err); ok {
		return apperr.FailedPrecondition(apperr.ReasonSSOConnectionInUse,
			inUse.Domain+" requires SSO and this org's connection signs it in; every org that requires it must turn Require SSO off first")
	}
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
	case errors.Is(err, coreorgs.ErrDomainSSONotSeen):
		return apperr.FailedPrecondition(apperr.ReasonDomainSSONotSeen, "sign in once through SSO with an account on this domain before requiring it")
	case errors.Is(err, coreorgs.ErrSSOConnectionsDisabled):
		return apperr.FailedPrecondition(apperr.ReasonSSOConnectionsDisabled, "SSO connections are not enabled on this server")
	case errors.Is(err, coreorgs.ErrSSOConnectionNotFound):
		return apperr.NotFound(apperr.ReasonSSOConnectionNotFound, "SSO connection not found")
	case errors.Is(err, coreorgs.ErrSSOConnectionLimitReached):
		return apperr.FailedPrecondition(apperr.ReasonSSOConnectionLimitReached, "an org can add at most 10 SSO connections")
	case errors.Is(err, coreorgs.ErrSSOConnectionIssuerInvalid):
		return apperr.Invalid(apperr.ReasonSSOConnectionIssuerInvalid, "enter an HTTPS issuer URL")
	case errors.Is(err, coreorgs.ErrSSOConnectionGoogleIssuer):
		return apperr.Invalid(apperr.ReasonSSOConnectionIssuerInvalid, "Google needs no connection: verify the domain and people sign in with Google")
	case errors.Is(err, coreorgs.ErrSSOConnectionSecretRequired):
		return apperr.Invalid(apperr.ReasonSSOConnectionSecretRequired, "enter the client secret again; the stored one can't be used")
	case errors.Is(err, coreorgs.ErrSSOConnectionDiscovery):
		return apperr.FailedPrecondition(apperr.ReasonSSOConnectionDiscoveryFailed, "we couldn't read the issuer's OpenID configuration; check the issuer URL")
	case errors.Is(err, coreorgs.ErrSSOConnectionIssuerMismatch):
		return apperr.Invalid(apperr.ReasonSSOConnectionIssuerInvalid, "enter the issuer exactly as its OpenID configuration names it, including any trailing slash")
	case errors.Is(err, coreorgs.ErrSSOConnectionPrivateIssuer):
		return apperr.FailedPrecondition(apperr.ReasonSSOConnectionDiscoveryFailed, "the issuer is on a private network address, which this server doesn't reach")
	case errors.Is(err, coreorgs.ErrDomainHasSSOConnection):
		return apperr.FailedPrecondition(apperr.ReasonDomainSSOConnectionTaken, "another SSO connection already signs in this domain")
	case errors.Is(err, coreorgs.ErrDNSUnavailable):
		return apperr.Unavailable(apperr.ReasonDomainLookupFailed, "we couldn't reach DNS to check the record; try again in a minute")
	default:
		return connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
}
