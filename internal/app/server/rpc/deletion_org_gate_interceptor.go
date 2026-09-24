package rpc

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"github.com/pug-sh/pug/internal/apperr"
	"github.com/pug-sh/pug/internal/core/deletion"
	"github.com/pug-sh/pug/internal/gen/proto/dashboard/billing/v1/billingv1connect"
	"github.com/pug-sh/pug/internal/gen/proto/dashboard/instance/v1/instancev1connect"
	"github.com/pug-sh/pug/internal/gen/proto/dashboard/orgemailproviders/v1/orgemailprovidersv1connect"
	"github.com/pug-sh/pug/internal/gen/proto/dashboard/orgs/v1/orgsv1connect"
	"github.com/pug-sh/pug/internal/gen/proto/dashboard/projects/v1/projectsv1connect"
)

var organizationWriteProcedures = map[string]struct{}{
	instancev1connect.InstanceAdminServiceRenameOrganizationProcedure:    {},
	instancev1connect.InstanceAdminServiceInviteMemberProcedure:          {},
	instancev1connect.InstanceAdminServiceResendInvitationProcedure:      {},
	instancev1connect.InstanceAdminServiceRevokeInvitationProcedure:      {},
	instancev1connect.InstanceAdminServiceSetMemberRoleProcedure:         {},
	instancev1connect.InstanceAdminServiceRemoveMemberProcedure:          {},
	orgsv1connect.OrgsServiceLeaveProcedure:                              {},
	orgsv1connect.OrgsServiceUpdateDisplayNameProcedure:                  {},
	orgsv1connect.OrgsServiceInviteMemberProcedure:                       {},
	orgsv1connect.OrgsServiceResendInviteProcedure:                       {},
	orgsv1connect.OrgsServiceRevokeInviteProcedure:                       {},
	orgsv1connect.OrgsServiceRemoveMemberProcedure:                       {},
	orgsv1connect.OrgsServiceUpdateMemberRoleProcedure:                   {},
	projectsv1connect.ProjectsServiceCreateProcedure:                     {},
	projectsv1connect.ProjectsServiceRetryDeletionProcedure:              {},
	orgemailprovidersv1connect.OrgEmailProvidersServiceSetProcedure:      {},
	orgemailprovidersv1connect.OrgEmailProvidersServiceRemoveProcedure:   {},
	orgemailprovidersv1connect.OrgEmailProvidersServiceSendTestProcedure: {},
	billingv1connect.BillingServiceCreateCheckoutSessionProcedure:        {},
	billingv1connect.BillingServiceCreatePortalSessionProcedure:          {},
	billingv1connect.BillingServiceConfirmCheckoutProcedure:              {},
}

func isOrganizationWriteProcedure(procedure string) bool {
	_, ok := organizationWriteProcedures[procedure]
	return ok
}

type organizationGateInterceptor struct{ gate *deletion.Gate }

func OrganizationGateInterceptor(gate *deletion.Gate) connect.Interceptor {
	return &organizationGateInterceptor{gate: gate}
}

func (i *organizationGateInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if !isOrganizationWriteProcedure(req.Spec().Procedure) {
			return next(ctx, req)
		}
		msg, ok := req.Any().(interface{ GetOrgId() string })
		if !ok || msg.GetOrgId() == "" {
			return next(ctx, req)
		}
		var response connect.AnyResponse
		err := i.gate.WithActiveOrganization(ctx, msg.GetOrgId(), func(ctx context.Context) error { var callErr error; response, callErr = next(ctx, req); return callErr })
		if errors.Is(err, deletion.ErrOrganizationInactive) {
			return nil, apperr.NotFound(apperr.ReasonOrgNotFound, "organization not found")
		}
		return response, err
	}
}
func (i *organizationGateInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}
func (i *organizationGateInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}
