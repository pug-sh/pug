package mcpgrants

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pug-sh/pug/internal/cimd"
	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
	"github.com/rs/xid"
)

// AuthorizationRequest is the query of /oauth/authorize. Its scope is ignored:
// every grant is read.
type AuthorizationRequest struct {
	ClientID            string
	CodeChallenge       string
	CodeChallengeMethod string
	RedirectURI         string
	Resource            string
	ResponseType        string
	State               string
}

// A RedirectError is a bad request whose redirect is trusted, so the person can go
// back to the client with it. URL carries the error, the state and iss.
type RedirectError struct {
	Err *Error
	URL string
}

func (e *RedirectError) Error() string { return e.Err.Error() }
func (e *RedirectError) Unwrap() error { return e.Err }

type Approval struct {
	Request    AuthorizationRequest
	CustomerID string
	// SessionID is the approving access token's sid, its refresh family.
	SessionID string
}

// Consent is what the consent page shows for a request that checks out.
type Consent struct {
	// ClientHost is the client_id URL's host, which names the app.
	ClientHost string
	// ClientName is the app's name for itself, which anyone can write.
	ClientName   string
	RedirectHost string
	Loopback     bool
	// Projects are what the grant reads, with any added to their orgs later.
	Projects []Project
	// CancelURL sends the browser back with access_denied.
	CancelURL string
}

// Project is a project with its org's name.
type Project struct {
	ID             string
	DisplayName    string
	OrgID          string
	OrgDisplayName string
}

// An S256 code_challenge is always 43 base64url characters.
var challengeShape = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// Describe checks the request for the consent page, which lists every live project
// in every org the person belongs to.
func (s *Service) Describe(ctx context.Context, customerID string, req AuthorizationRequest) (Consent, error) {
	clientName, err := s.checkRequest(ctx, req)
	if err != nil {
		return Consent{}, err
	}
	rows, err := dbread.New(s.pgW).GetProjectsWithOrgByCustomerID(ctx, customerID)
	if err != nil {
		return Consent{}, recordServiceError(ctx, "failed to list the projects to connect", err, slog.String("customer_id", customerID))
	}
	projects := make([]Project, len(rows))
	for i, r := range rows {
		projects[i] = Project{ID: r.ID, DisplayName: r.DisplayName, OrgID: r.OrgID, OrgDisplayName: r.OrgDisplayName}
	}
	return Consent{
		ClientHost:   host(req.ClientID),
		ClientName:   clientName,
		RedirectHost: host(req.RedirectURI),
		Loopback:     cimd.IsLoopback(req.RedirectURI),
		Projects:     projects,
		CancelURL:    s.redirect(req, url.Values{"error": {ErrAccessDenied.Code()}}),
	}, nil
}

// host is the host of a URL that checkRequest accepted, so it parses.
func host(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Host
}

// Approve creates a grant and its code, and returns the URL to send the browser to.
func (s *Service) Approve(ctx context.Context, a Approval) (string, error) {
	clientName, err := s.checkRequest(ctx, a.Request)
	if err != nil {
		return "", err
	}
	code, err := s.createGrant(ctx, a, clientName)
	if err != nil {
		return "", err
	}
	return s.redirect(a.Request, url.Values{"code": {code}}), nil
}

// checkRequest returns the client's name for itself. Until the redirect checks out
// an error goes nowhere; after that it is a *RedirectError.
func (s *Service) checkRequest(ctx context.Context, req AuthorizationRequest) (string, error) {
	doc, err := s.documents.Fetch(ctx, req.ClientID)
	switch {
	case errors.Is(err, cimd.ErrUnavailable):
		return "", fmt.Errorf("%w: %w", ErrClientUnavailable, err)
	case err != nil:
		return "", fmt.Errorf("%w: %w", ErrClientInvalid, err)
	case !doc.Lists(req.RedirectURI):
		err = ErrRedirectNotListed
	case req.ResponseType != "code":
		err = s.redirectError(req, ErrUnsupportedResponseType)
	case req.CodeChallengeMethod != "S256", !challengeShape.MatchString(req.CodeChallenge):
		err = s.redirectError(req, ErrInvalidRequest)
	case !s.allowsResource(req.Resource):
		err = s.redirectError(req, ErrInvalidTarget)
	default:
		return doc.ClientName, nil
	}
	slog.WarnContext(ctx, "authorization request refused", slogx.Error(err), slog.String("client_id", req.ClientID),
		slog.String("redirect_uri", req.RedirectURI), slog.String("response_type", req.ResponseType),
		slog.String("code_challenge_method", req.CodeChallengeMethod), slog.String("resource", req.Resource))
	return "", err
}

func (s *Service) redirectError(req AuthorizationRequest, oauthErr *Error) error {
	return &RedirectError{Err: oauthErr, URL: s.redirect(req, url.Values{"error": {oauthErr.Code()}})}
}

// redirect adds params, the state and iss to the request's redirect_uri. ChatGPT
// keeps a stable client_id only when every redirect carries iss (RFC 9207).
func (s *Service) redirect(req AuthorizationRequest, params url.Values) string {
	if req.State != "" {
		params.Set("state", req.State)
	}
	params.Set("iss", s.issuer)
	sep := "?"
	if strings.Contains(req.RedirectURI, "?") {
		sep = "&"
	}
	return req.RedirectURI + sep + params.Encode()
}

// createGrant records the approval and returns its code. A signed-out session can't
// approve, and its proven domain carries onto the grant.
func (s *Service) createGrant(ctx context.Context, a Approval, clientName string) (string, error) {
	r := dbread.New(s.pgW)
	provenDomain, err := r.GetLiveSessionProvenDomain(ctx, dbread.GetLiveSessionProvenDomainParams{
		FamilyID: a.SessionID, CustomerID: a.CustomerID,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", ErrSessionEnded
	case err != nil:
		return "", recordServiceError(ctx, "failed to read the approving session", err, slog.String("customer_id", a.CustomerID))
	}
	email, err := r.GetCustomerEmailByID(ctx, a.CustomerID)
	if err != nil {
		return "", recordServiceError(ctx, "failed to read the approving customer", err, slog.String("customer_id", a.CustomerID))
	}
	if err := coreorgs.CheckSignInInTx(ctx, r, email, provenDomain.String); err != nil {
		return "", err
	}
	// With no org the grant reads nothing, and its client would loop on reconnecting.
	orgs, err := r.GetOrgsByCustomerID(ctx, a.CustomerID)
	switch {
	case err != nil:
		return "", recordServiceError(ctx, "failed to read the approving customer's orgs", err, slog.String("customer_id", a.CustomerID))
	case len(orgs) == 0:
		return "", ErrNoOrg
	}

	code := newToken("")
	if err := dbwrite.New(s.pgW).CreateMCPGrant(ctx, dbwrite.CreateMCPGrantParams{
		ClientID:      a.Request.ClientID,
		ClientName:    clientName,
		CodeChallenge: a.Request.CodeChallenge,
		CodeExpiresAt: postgres.NewTimestamptz(time.Now().Add(codeTTL)),
		CodeHash:      hashToken(code),
		CustomerID:    a.CustomerID,
		ID:            xid.New().String(),
		ProvenDomain:  provenDomain,
		RedirectUri:   a.Request.RedirectURI,
	}); err != nil {
		return "", recordServiceError(ctx, "failed to create mcp grant", err, slog.String("customer_id", a.CustomerID))
	}
	return code, nil
}
