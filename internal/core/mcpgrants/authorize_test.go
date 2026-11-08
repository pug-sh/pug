package mcpgrants

import (
	"context"
	"errors"
	"net/url"
	"testing"

	"golang.org/x/oauth2"

	"github.com/pug-sh/pug/internal/cimd"
)

const (
	testIssuer = "https://api.pugs.dev"

	claudeID       = "https://claude.ai/oauth/mcp-oauth-client-metadata"
	claudeRedirect = "https://claude.ai/api/mcp/auth_callback"
	chatGPTID      = "https://chatgpt.com/oauth/client.json"
)

type fetchFunc func(clientID string) (cimd.Document, error)

func (f fetchFunc) Fetch(_ context.Context, clientID string) (cimd.Document, error) {
	return f(clientID)
}

// claude serves claude.ai's document, as cimd returns it, and nothing else.
var claude = fetchFunc(func(clientID string) (cimd.Document, error) {
	if clientID != claudeID {
		return cimd.Document{}, cimd.ErrUnavailable
	}
	return cimd.Document{ClientID: claudeID, ClientName: "Claude", RedirectURIs: []string{claudeRedirect}}, nil
})

func request(clientID, redirect string) AuthorizationRequest {
	return AuthorizationRequest{
		ClientID:            clientID,
		CodeChallenge:       oauth2.S256ChallengeFromVerifier(oauth2.GenerateVerifier()),
		CodeChallengeMethod: "S256",
		RedirectURI:         redirect,
		Resource:            testIssuer + "/mcp",
		ResponseType:        "code",
		State:               "state-1",
	}
}

func TestClientErrors(t *testing.T) {
	for _, tc := range []struct{ fetched, want error }{
		{cimd.ErrClientID, ErrClientInvalid},
		{cimd.ErrDocument, ErrClientInvalid},
		{cimd.ErrUnavailable, ErrClientUnavailable},
	} {
		s := NewService(nil, fetchFunc(func(string) (cimd.Document, error) { return cimd.Document{}, tc.fetched }), testIssuer)
		if _, err := s.checkRequest(t.Context(), request(claudeID, claudeRedirect)); !errors.Is(err, tc.want) {
			t.Errorf("fetch error %v: err = %v, want %v", tc.fetched, err, tc.want)
		}
	}
}

func TestBadRequestsGoBackToTheClient(t *testing.T) {
	s := NewService(nil, claude, testIssuer)
	for _, tc := range []struct {
		name   string
		modify func(*AuthorizationRequest)
		want   *Error
	}{
		{"response_type token", func(r *AuthorizationRequest) { r.ResponseType = "token" }, ErrUnsupportedResponseType},
		{"plain PKCE", func(r *AuthorizationRequest) { r.CodeChallengeMethod = "plain" }, ErrInvalidRequest},
		{"no PKCE", func(r *AuthorizationRequest) { r.CodeChallenge, r.CodeChallengeMethod = "", "" }, ErrInvalidRequest},
		{"a short challenge", func(r *AuthorizationRequest) { r.CodeChallenge = r.CodeChallenge[:42] }, ErrInvalidRequest},
		{"a challenge outside base64url", func(r *AuthorizationRequest) { r.CodeChallenge = r.CodeChallenge[:42] + "+" }, ErrInvalidRequest},
		{"another resource", func(r *AuthorizationRequest) { r.Resource = "https://evil.example/mcp" }, ErrInvalidTarget},
		{"the slash form of the resource", func(r *AuthorizationRequest) { r.Resource = testIssuer + "/mcp/" }, nil},
		{"no resource", func(r *AuthorizationRequest) { r.Resource = "" }, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := request(claudeID, claudeRedirect)
			tc.modify(&req)
			// A redirect the document doesn't list is never sent an error.
			untrusted := req
			untrusted.RedirectURI = "https://evil.example/callback"
			if _, err := s.checkRequest(t.Context(), untrusted); !errors.Is(err, ErrRedirectNotListed) {
				t.Fatalf("to an unlisted redirect: err = %v, want ErrRedirectNotListed", err)
			}
			name, err := s.checkRequest(t.Context(), req)
			if tc.want == nil {
				if err != nil || name != "Claude" {
					t.Fatalf("name = %q, err = %v; want Claude", name, err)
				}
				return
			}
			redirectErr, isRedirect := errors.AsType[*RedirectError](err)
			if !isRedirect || !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want a RedirectError for %v", err, tc.want)
			}
			u, err := url.Parse(redirectErr.URL)
			if err != nil {
				t.Fatal(err)
			}
			q := u.Query()
			if u.Scheme+"://"+u.Host+u.Path != claudeRedirect || q.Get("error") != tc.want.Code() ||
				q.Get("state") != "state-1" || q.Get("iss") != testIssuer {
				t.Fatalf("redirect = %s, want %s with error %s, the state and iss", redirectErr.URL, claudeRedirect, tc.want.Code())
			}
		})
	}
}
