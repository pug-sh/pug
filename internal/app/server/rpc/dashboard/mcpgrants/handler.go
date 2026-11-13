package mcpgrants

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/pug-sh/pug/internal/app/server/rpc"
	"github.com/pug-sh/pug/internal/apperr"
	coremcpgrants "github.com/pug-sh/pug/internal/core/mcpgrants"
	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	mcpgrantsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/mcpgrants/v1"
)

type Server struct {
	grants *coremcpgrants.Service
}

// NewServer takes a nil service when OAuth is off. The service is mounted anyway,
// because every service in the authz registry must be.
func NewServer(grants *coremcpgrants.Service) *Server { return &Server{grants: grants} }

func (s *Server) requireOAuth() error {
	if s.grants == nil {
		return apperr.FailedPrecondition(apperr.ReasonMCPOAuthDisabled, "OAuth for MCP is not enabled on this server")
	}
	return nil
}

func (s *Server) DescribeAuthorization(
	ctx context.Context,
	req *connect.Request[mcpgrantsv1.DescribeAuthorizationRequest],
) (*connect.Response[mcpgrantsv1.DescribeAuthorizationResponse], error) {
	principal, err := rpc.MustGetPrincipalWithCustomer(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.requireOAuth(); err != nil {
		return nil, err
	}
	c, err := s.grants.Describe(ctx, principal.Customer.ID, authorizationRequest(req.Msg.GetRequest()))
	if err != nil {
		return nil, approvalError(err)
	}
	projects := make([]*mcpgrantsv1.Project, len(c.Projects))
	for i, p := range c.Projects {
		projects[i] = projectToRPC(p)
	}
	return connect.NewResponse(&mcpgrantsv1.DescribeAuthorizationResponse{
		ClientHost:       proto.String(c.ClientHost),
		ClientName:       proto.String(c.ClientName),
		RedirectHost:     proto.String(c.RedirectHost),
		LoopbackRedirect: proto.Bool(c.Loopback),
		Projects:         projects,
		CancelUri:        proto.String(c.CancelURL),
	}), nil
}

func (s *Server) ApproveAuthorization(
	ctx context.Context,
	req *connect.Request[mcpgrantsv1.ApproveAuthorizationRequest],
) (*connect.Response[mcpgrantsv1.ApproveAuthorizationResponse], error) {
	principal, err := rpc.MustGetPrincipalWithCustomer(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.requireOAuth(); err != nil {
		return nil, err
	}
	redirect, err := s.grants.Approve(ctx, coremcpgrants.Approval{
		Request:    authorizationRequest(req.Msg.GetRequest()),
		CustomerID: principal.Customer.ID,
		SessionID:  principal.SessionID,
	})
	if err != nil {
		return nil, approvalError(err)
	}
	return connect.NewResponse(&mcpgrantsv1.ApproveAuthorizationResponse{RedirectUri: proto.String(redirect)}), nil
}

func (s *Server) ListGrants(
	ctx context.Context,
	_ *connect.Request[mcpgrantsv1.ListGrantsRequest],
) (*connect.Response[mcpgrantsv1.ListGrantsResponse], error) {
	principal, err := rpc.MustGetPrincipalWithCustomer(ctx)
	if err != nil {
		return nil, err
	}
	if s.grants == nil {
		return connect.NewResponse(&mcpgrantsv1.ListGrantsResponse{OauthEnabled: proto.Bool(false)}), nil
	}
	grants, err := s.grants.ListGrants(ctx, principal.Customer.ID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	out := make([]*mcpgrantsv1.Grant, len(grants))
	for i, g := range grants {
		out[i] = grantToRPC(g)
	}
	return connect.NewResponse(&mcpgrantsv1.ListGrantsResponse{OauthEnabled: proto.Bool(true), Grants: out}), nil
}

func (s *Server) RevokeGrant(
	ctx context.Context,
	req *connect.Request[mcpgrantsv1.RevokeGrantRequest],
) (*connect.Response[mcpgrantsv1.RevokeGrantResponse], error) {
	principal, err := rpc.MustGetPrincipalWithCustomer(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.requireOAuth(); err != nil {
		return nil, err
	}
	if err := s.grants.RevokeGrant(ctx, principal.Customer.ID, req.Msg.GetGrantId()); err != nil {
		if errors.Is(err, coremcpgrants.ErrGrantNotFound) {
			return nil, apperr.NotFound(apperr.ReasonMCPGrantNotFound, "grant not found", apperr.Resource("mcp_grant", req.Msg.GetGrantId()))
		}
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	return connect.NewResponse(&mcpgrantsv1.RevokeGrantResponse{}), nil
}

// approvalError translates what Describe and Approve return. The service logged
// and recorded anything unexpected.
func approvalError(err error) error {
	if redirectErr, ok := errors.AsType[*coremcpgrants.RedirectError](err); ok {
		return apperr.Invalid(apperr.ReasonMCPAuthorizationInvalid, "the app's authorization request is invalid",
			apperr.Detail(&mcpgrantsv1.AuthorizationError{
				Error:       proto.String(redirectErr.Err.Code()),
				RedirectUri: proto.String(redirectErr.URL),
			}))
	}
	// Unauthenticated, so the app refreshes the session, whose refusal names the
	// providers that can sign the domain in.
	if ssoErr, ok := errors.AsType[*coreorgs.SSORequiredError](err); ok {
		return apperr.Unauthenticated(apperr.ReasonSSORequired, ssoErr.Domain+" accounts sign in through SSO")
	}
	switch {
	case errors.Is(err, coremcpgrants.ErrClientUnavailable):
		return apperr.FailedPrecondition(apperr.ReasonMCPClientUnavailable, "the app's details couldn't be loaded")
	case errors.Is(err, coremcpgrants.ErrClientInvalid):
		return apperr.Invalid(apperr.ReasonMCPClientInvalid, "the app's client_id or its details are invalid")
	case errors.Is(err, coremcpgrants.ErrRedirectNotListed):
		return apperr.Invalid(apperr.ReasonMCPRedirectNotListed, "the app's details don't list its redirect_uri on a host pug accepts")
	// The app refreshes the session and retries, and the new access token has a sid.
	case errors.Is(err, coremcpgrants.ErrSessionEnded):
		return apperr.Unauthenticated(apperr.ReasonUnauthenticated, "the session has ended")
	case errors.Is(err, coremcpgrants.ErrNoOrg):
		return apperr.FailedPrecondition(apperr.ReasonMCPNoOrg, "you aren't in any org, so there is nothing to connect")
	}
	return connect.NewError(connect.CodeInternal, errors.New("internal error"))
}
