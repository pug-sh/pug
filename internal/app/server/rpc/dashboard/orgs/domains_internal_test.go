package orgs

import (
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"github.com/pug-sh/pug/internal/apperr"
	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
)

func TestDomainErrorMapsSSOConnectionErrors(t *testing.T) {
	for _, tc := range []struct {
		err    error
		code   connect.Code
		reason apperr.Reason
	}{
		{&coreorgs.SSOConnectionInUseError{Domain: "acme.com"}, connect.CodeFailedPrecondition, apperr.ReasonSSOConnectionInUse},
		{coreorgs.ErrSSOConnectionsDisabled, connect.CodeFailedPrecondition, apperr.ReasonSSOConnectionsDisabled},
		{coreorgs.ErrSSOConnectionNotFound, connect.CodeNotFound, apperr.ReasonSSOConnectionNotFound},
		{coreorgs.ErrSSOConnectionLimitReached, connect.CodeFailedPrecondition, apperr.ReasonSSOConnectionLimitReached},
		{coreorgs.ErrSSOConnectionIssuerInvalid, connect.CodeInvalidArgument, apperr.ReasonSSOConnectionIssuerInvalid},
		{coreorgs.ErrSSOConnectionGoogleIssuer, connect.CodeInvalidArgument, apperr.ReasonSSOConnectionIssuerInvalid},
		{coreorgs.ErrSSOConnectionSecretRequired, connect.CodeInvalidArgument, apperr.ReasonSSOConnectionSecretRequired},
		{coreorgs.ErrSSOConnectionDiscovery, connect.CodeFailedPrecondition, apperr.ReasonSSOConnectionDiscoveryFailed},
		{coreorgs.ErrSSOConnectionIssuerMismatch, connect.CodeInvalidArgument, apperr.ReasonSSOConnectionIssuerInvalid},
		{coreorgs.ErrSSOConnectionPrivateIssuer, connect.CodeFailedPrecondition, apperr.ReasonSSOConnectionDiscoveryFailed},
		{coreorgs.ErrDomainHasSSOConnection, connect.CodeFailedPrecondition, apperr.ReasonDomainSSOConnectionTaken},
	} {
		ae, ok := errors.AsType[*apperr.Error](domainError(tc.err, "o"))
		if !ok || ae.Code() != tc.code || ae.Reason() != tc.reason {
			t.Errorf("%v: got %v, want %v / %s", tc.err, ae, tc.code, tc.reason)
		}
	}
	if err := domainError(&coreorgs.SSOConnectionInUseError{Domain: "acme.com"}, "o"); !strings.Contains(err.Error(), "acme.com") {
		t.Errorf("in-use message %q must name the domain", err)
	}
}
