package mcpgrants

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"testing"
	"time"

	"connectrpc.com/authn"
	"connectrpc.com/connect"
	"github.com/rs/xid"
	"golang.org/x/oauth2"
	"google.golang.org/protobuf/proto"

	"github.com/pug-sh/pug/internal/app/server/rpc"
	"github.com/pug-sh/pug/internal/apperr"
	"github.com/pug-sh/pug/internal/cimd"
	coremcpgrants "github.com/pug-sh/pug/internal/core/mcpgrants"
	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	"github.com/pug-sh/pug/internal/deps/postgres"
	mcpgrantsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/mcpgrants/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
)

const (
	issuer         = "https://api.pugs.dev"
	claudeID       = "https://claude.ai/oauth/mcp-oauth-client-metadata"
	claudeRedirect = "https://claude.ai/api/mcp/auth_callback"
)

type fetchFunc func(clientID string) (cimd.Document, error)

func (f fetchFunc) Fetch(_ context.Context, clientID string) (cimd.Document, error) {
	return f(clientID)
}

var claude = fetchFunc(func(clientID string) (cimd.Document, error) {
	if clientID != claudeID {
		return cimd.Document{}, cimd.ErrUnavailable
	}
	return cimd.Document{ClientID: claudeID, ClientName: "Claude", RedirectURIs: []string{claudeRedirect}}, nil
})

func signedIn(c dbread.Customer, sessionID string) context.Context {
	return authn.SetInfo(context.Background(), &rpc.Principal{AuthType: rpc.AuthTypeJWT, Customer: &c, SessionID: sessionID})
}

func goodRequest(verifier string) *mcpgrantsv1.AuthorizationRequest {
	return &mcpgrantsv1.AuthorizationRequest{
		ClientId:            proto.String(claudeID),
		CodeChallenge:       proto.String(oauth2.S256ChallengeFromVerifier(verifier)),
		CodeChallengeMethod: proto.String("S256"),
		RedirectUri:         proto.String(claudeRedirect),
		Resource:            proto.String(issuer + "/mcp"),
		ResponseType:        proto.String("code"),
		State:               proto.String("state-1"),
	}
}

func wantAppErr(t *testing.T, err error, code connect.Code, reason apperr.Reason) {
	t.Helper()
	ae, ok := errors.AsType[*apperr.Error](err)
	if !ok || ae.Code() != code || ae.Reason() != reason {
		t.Fatalf("err = %v, want %v / %s", err, code, reason)
	}
}

func TestOAuthOff(t *testing.T) {
	srv := NewServer(nil)
	ctx := signedIn(dbread.Customer{ID: "c0000000000000000000"}, "")

	_, err := srv.DescribeAuthorization(ctx, connect.NewRequest(&mcpgrantsv1.DescribeAuthorizationRequest{}))
	wantAppErr(t, err, connect.CodeFailedPrecondition, apperr.ReasonMCPOAuthDisabled)
	_, err = srv.ApproveAuthorization(ctx, connect.NewRequest(&mcpgrantsv1.ApproveAuthorizationRequest{}))
	wantAppErr(t, err, connect.CodeFailedPrecondition, apperr.ReasonMCPOAuthDisabled)
	_, err = srv.RevokeGrant(ctx, connect.NewRequest(&mcpgrantsv1.RevokeGrantRequest{}))
	wantAppErr(t, err, connect.CodeFailedPrecondition, apperr.ReasonMCPOAuthDisabled)

	resp, err := srv.ListGrants(ctx, connect.NewRequest(&mcpgrantsv1.ListGrantsRequest{}))
	if err != nil || resp.Msg.OauthEnabled == nil || resp.Msg.GetOauthEnabled() || len(resp.Msg.GetGrants()) != 0 {
		t.Fatalf("ListGrants = %v, %v; want oauth_enabled false", resp, err)
	}
}

func TestApprovalErrors(t *testing.T) {
	for _, tc := range []struct {
		err    error
		code   connect.Code
		reason apperr.Reason
	}{
		{fmt.Errorf("%w: %w", coremcpgrants.ErrClientUnavailable, cimd.ErrUnavailable), connect.CodeFailedPrecondition, apperr.ReasonMCPClientUnavailable},
		{fmt.Errorf("%w: %w", coremcpgrants.ErrClientInvalid, cimd.ErrDocument), connect.CodeInvalidArgument, apperr.ReasonMCPClientInvalid},
		{coremcpgrants.ErrRedirectNotListed, connect.CodeInvalidArgument, apperr.ReasonMCPRedirectNotListed},
		{&coremcpgrants.RedirectError{Err: coremcpgrants.ErrInvalidRequest}, connect.CodeInvalidArgument, apperr.ReasonMCPAuthorizationInvalid},
		{coremcpgrants.ErrSessionEnded, connect.CodeUnauthenticated, apperr.ReasonUnauthenticated},
		{coremcpgrants.ErrNoOrg, connect.CodeFailedPrecondition, apperr.ReasonMCPNoOrg},
		{&coreorgs.SSORequiredError{Domain: "acme.com"}, connect.CodeUnauthenticated, apperr.ReasonSSORequired},
	} {
		wantAppErr(t, approvalError(tc.err), tc.code, tc.reason)
	}
	if err := approvalError(errors.New("boom")); connect.CodeOf(err) != connect.CodeInternal {
		t.Errorf("an unexpected error = %v, want Internal", err)
	}
}

type backend struct {
	srv      *Server
	svc      *coremcpgrants.Service
	customer dbread.Customer
	org      string
	project  string
	session  string
}

func newBackend(t *testing.T) backend {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	ctx := t.Context()
	id := xid.New().String()
	if _, err := dbwrite.New(db.PgW).CreateCustomer(ctx, dbwrite.CreateCustomerParams{ID: id, Email: "bob@acme.com"}); err != nil {
		t.Fatal(err)
	}
	customer, err := dbread.New(db.PgW).GetCustomerByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	org, err := coreorgs.NewService(db.PgRO, db.PgW, nil).CreateOrgWithDefaults(ctx, id, "Acme")
	if err != nil {
		t.Fatal(err)
	}
	projects, err := dbread.New(db.PgW).GetProjectsByOrgID(ctx, org.ID)
	if err != nil || len(projects) != 1 {
		t.Fatalf("projects = %v, %v", projects, err)
	}
	session := xid.New().String()
	if _, err := dbwrite.New(db.PgW).CreateRefreshToken(ctx, dbwrite.CreateRefreshTokenParams{
		ID: xid.New().String(), CustomerID: id, FamilyID: session, TokenHash: session,
		ExpiresAt: postgres.NewTimestamptz(time.Now().Add(time.Hour)),
	}); err != nil {
		t.Fatal(err)
	}
	svc := coremcpgrants.NewService(db.PgW, claude, issuer)
	return backend{srv: NewServer(svc), svc: svc, customer: customer, org: org.ID, project: projects[0].ID, session: session}
}

func (b backend) describe(req *mcpgrantsv1.AuthorizationRequest) (*mcpgrantsv1.DescribeAuthorizationResponse, error) {
	resp, err := b.srv.DescribeAuthorization(signedIn(b.customer, b.session),
		connect.NewRequest(&mcpgrantsv1.DescribeAuthorizationRequest{Request: req}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (b backend) approve(req *mcpgrantsv1.AuthorizationRequest) (*mcpgrantsv1.ApproveAuthorizationResponse, error) {
	resp, err := b.srv.ApproveAuthorization(signedIn(b.customer, b.session),
		connect.NewRequest(&mcpgrantsv1.ApproveAuthorizationRequest{Request: req}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// redirectQuery checks that redirect goes back to claude.ai with iss, and returns
// its query.
func redirectQuery(t *testing.T, what, redirect string) url.Values {
	t.Helper()
	u, err := url.Parse(redirect)
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	q := u.Query()
	if u.Scheme+"://"+u.Host+u.Path != claudeRedirect || q.Get("iss") != issuer || q.Get("state") != "state-1" {
		t.Fatalf("%s = %s, want %s with the state and iss", what, redirect, claudeRedirect)
	}
	return q
}

func TestEveryRedirectCarriesIss(t *testing.T) {
	b := newBackend(t)
	verifier := oauth2.GenerateVerifier()

	consent, err := b.describe(goodRequest(verifier))
	if err != nil {
		t.Fatalf("DescribeAuthorization: %v", err)
	}
	if consent.GetClientHost() != "claude.ai" || len(consent.GetProjects()) != 1 || consent.GetProjects()[0].GetId() != b.project {
		t.Fatalf("consent = %v, want claude.ai and bob's project", consent)
	}
	if q := redirectQuery(t, "cancel_uri", consent.GetCancelUri()); q.Get("error") != "access_denied" {
		t.Errorf("cancel_uri error = %q, want access_denied", q.Get("error"))
	}

	approved, err := b.approve(goodRequest(verifier))
	if err != nil {
		t.Fatalf("ApproveAuthorization: %v", err)
	}
	if q := redirectQuery(t, "redirect_uri", approved.GetRedirectUri()); q.Get("code") == "" {
		t.Error("redirect_uri has no code")
	}

	bad := goodRequest(verifier)
	bad.ResponseType = proto.String("token")
	for name, call := range map[string]func() error{
		"DescribeAuthorization": func() error { _, err := b.describe(bad); return err },
		"ApproveAuthorization":  func() error { _, err := b.approve(bad); return err },
	} {
		err := call()
		wantAppErr(t, err, connect.CodeInvalidArgument, apperr.ReasonMCPAuthorizationInvalid)
		ae, _ := errors.AsType[*apperr.Error](err)
		var detail *mcpgrantsv1.AuthorizationError
		for _, d := range ae.Details() {
			if a, ok := d.(*mcpgrantsv1.AuthorizationError); ok {
				detail = a
			}
		}
		if detail.GetError() != "unsupported_response_type" {
			t.Fatalf("%s: detail = %v, want unsupported_response_type", name, detail)
		}
		redirectQuery(t, name+" error redirect_uri", detail.GetRedirectUri())
	}
}

func TestListAndRevokeGrants(t *testing.T) {
	b := newBackend(t)
	ctx := signedIn(b.customer, b.session)
	verifier := oauth2.GenerateVerifier()
	approved, err := b.approve(goodRequest(verifier))
	if err != nil {
		t.Fatalf("ApproveAuthorization: %v", err)
	}
	if _, err := b.svc.ExchangeCode(t.Context(), coremcpgrants.CodeExchange{
		ClientID: claudeID, Code: redirectQuery(t, "redirect_uri", approved.GetRedirectUri()).Get("code"),
		CodeVerifier: verifier, RedirectURI: claudeRedirect,
	}); err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}

	list := func() *mcpgrantsv1.ListGrantsResponse {
		t.Helper()
		resp, err := b.srv.ListGrants(ctx, connect.NewRequest(&mcpgrantsv1.ListGrantsRequest{}))
		if err != nil {
			t.Fatalf("ListGrants: %v", err)
		}
		return resp.Msg
	}
	listed := list()
	if !listed.GetOauthEnabled() || len(listed.GetGrants()) != 1 {
		t.Fatalf("ListGrants = %v, want OAuth on and one grant", listed)
	}
	g := listed.GetGrants()[0]
	if g.GetClientHost() != "claude.ai" || g.GetClientName() != "Claude" || len(g.GetOrgs()) != 1 ||
		g.GetOrgs()[0].GetId() != b.org || g.GetOrgs()[0].GetDisplayName() != "Acme" || g.GetCreateTime() == nil ||
		!g.GetLastUsedTime().AsTime().After(g.GetCreateTime().AsTime()) {
		t.Fatalf("grant = %v, want claude.ai on bob's org, used after it was created", g)
	}

	revoke := func() error {
		_, err := b.srv.RevokeGrant(ctx, connect.NewRequest(&mcpgrantsv1.RevokeGrantRequest{GrantId: proto.String(g.GetId())}))
		return err
	}
	if err := revoke(); err != nil {
		t.Fatalf("RevokeGrant: %v", err)
	}
	wantAppErr(t, revoke(), connect.CodeNotFound, apperr.ReasonMCPGrantNotFound)
	if n := len(list().GetGrants()); n != 0 {
		t.Fatalf("%d grants after the revoke, want none", n)
	}
}
