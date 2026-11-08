package cimd

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

const (
	claudeID       = "https://claude.ai/oauth/mcp-oauth-client-metadata"
	claudeRedirect = "https://claude.ai/api/mcp/auth_callback"
	claudeCodeID   = "https://claude.ai/oauth/claude-code-client-metadata"
	chatGPTID      = "https://chatgpt.com/oauth/client.json"
)

// The three documents as served on 2026-11-08.
var realDocuments = map[string]string{
	claudeID:     `{"client_id":"https://claude.ai/oauth/mcp-oauth-client-metadata","client_name":"Claude","client_uri":"https://claude.ai","redirect_uris":["https://claude.ai/api/mcp/auth_callback"],"grant_types":["authorization_code","refresh_token","urn:ietf:params:oauth:grant-type:jwt-bearer"],"response_types":["code"],"token_endpoint_auth_method":"none"}`,
	claudeCodeID: `{"client_id":"https://claude.ai/oauth/claude-code-client-metadata","client_name":"Claude Code","client_uri":"https://claude.ai","redirect_uris":["http://localhost/callback","http://127.0.0.1/callback"],"grant_types":["authorization_code","refresh_token"],"response_types":["code"],"token_endpoint_auth_method":"none"}`,
	chatGPTID:    `{"client_id":"https://chatgpt.com/oauth/client.json","client_uri":"https://chatgpt.com/","redirect_uris":["https://chatgpt.com/connector_platform_oauth_redirect"],"token_endpoint_auth_method":"private_key_jwt","token_endpoint_auth_methods_supported":["none","private_key_jwt"],"grant_types":["authorization_code","refresh_token"],"response_types":["code"],"client_name":"ChatGPT","logo_uri":"https://persistent.oaistatic.com/sonic/misc/openai-logo.png","token_endpoint_auth_signing_alg":"RS256","jwks_uri":"https://chatgpt.com/oauth/jwks.json"}`,
}

// documents serves each body with a 200, and anything else as a 404.
func documents(docs map[string]string) *Client {
	return serve(func(u string) (int, http.Header, string) {
		if body, ok := docs[u]; ok {
			return http.StatusOK, http.Header{}, body
		}
		return http.StatusNotFound, http.Header{}, ""
	})
}

func exampleDocument(clientID string, redirects ...string) string {
	return `{"client_id":"` + clientID + `","client_name":"Example","redirect_uris":["` + strings.Join(redirects, `","`) + `"]}`
}

func TestRealDocumentsPass(t *testing.T) {
	c := documents(realDocuments)
	for _, tc := range []struct{ clientID, redirect, name string }{
		{claudeID, claudeRedirect, "Claude"},
		{claudeCodeID, "http://localhost:53682/callback", "Claude Code"},
		{claudeCodeID, "http://127.0.0.1:53682/callback", "Claude Code"},
		{chatGPTID, "https://chatgpt.com/connector_platform_oauth_redirect", "ChatGPT"},
	} {
		doc, err := c.Fetch(t.Context(), tc.clientID)
		if err != nil || doc.ClientName != tc.name || !doc.Lists(tc.redirect) {
			t.Errorf("%s: name = %q, err = %v; want %q listing %s", tc.clientID, doc.ClientName, err, tc.name, tc.redirect)
		}
	}
}

func TestClientMustBeAbleToUseNone(t *testing.T) {
	for _, tc := range []struct{ only, clientID, from, to string }{
		{"private_key_jwt", chatGPTID, `["none","private_key_jwt"]`, `["private_key_jwt"]`},
		{"a secret", claudeID, `"token_endpoint_auth_method":"none"`, `"token_endpoint_auth_method":"client_secret_basic"`},
	} {
		doc := strings.Replace(realDocuments[tc.clientID], tc.from, tc.to, 1)
		if _, err := documents(map[string]string{tc.clientID: doc}).Fetch(t.Context(), tc.clientID); !errors.Is(err, ErrDocument) {
			t.Errorf("a client offering only %s: err = %v, want ErrDocument", tc.only, err)
		}
	}
}

func TestDocumentRefusals(t *testing.T) {
	const id = "https://example.com/client.json"
	for _, tc := range []struct {
		name string
		doc  string
		want error
	}{
		{
			name: "a redirect on another host",
			doc:  exampleDocument(id, "https://example.com/callback", "https://evil.example/callback"),
			want: ErrDocument,
		},
		{
			name: "http off loopback",
			doc:  exampleDocument(id, "http://example.com/callback"),
			want: ErrDocument,
		},
		{
			name: "a redirect with userinfo",
			doc:  exampleDocument(id, "https://user@example.com/callback"),
			want: ErrDocument,
		},
		{
			name: "another scheme on loopback",
			doc:  exampleDocument(id, "javascript://localhost/callback"),
			want: ErrDocument,
		},
		{
			name: "no redirect_uris",
			doc:  `{"client_id":"` + id + `","client_name":"Example"}`,
			want: ErrDocument,
		},
		{
			name: "grant_types without authorization_code",
			doc:  `{"client_id":"` + id + `","client_name":"Example","redirect_uris":["https://example.com/callback"],"grant_types":["refresh_token"]}`,
			want: ErrDocument,
		},
		{
			name: "response_types without code",
			doc:  `{"client_id":"` + id + `","client_name":"Example","redirect_uris":["https://example.com/callback"],"response_types":["token"]}`,
			want: ErrDocument,
		},
		{
			name: "no client_name",
			doc:  `{"client_id":"` + id + `","redirect_uris":["https://example.com/callback"]}`,
			want: ErrDocument,
		},
		{
			name: "a client_name over 200 characters",
			doc:  `{"client_id":"` + id + `","client_name":"` + strings.Repeat("é", 201) + `","redirect_uris":["https://example.com/callback"]}`,
			want: ErrDocument,
		},
		{
			name: "a client_name of 200 characters in 400 bytes",
			doc:  `{"client_id":"` + id + `","client_name":"` + strings.Repeat("é", 200) + `","redirect_uris":["https://example.com/callback"]}`,
		},
		{
			name: "a NUL in client_name",
			doc:  `{"client_id":"` + id + `","client_name":"Ex\u0000ample","redirect_uris":["https://example.com/callback"]}`,
			want: ErrDocument,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := documents(map[string]string{id: tc.doc}).Fetch(t.Context(), id); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestLoopbackRedirectMatchesAnyPort(t *testing.T) {
	const ipv6ID = "https://example.com/client.json"
	c := documents(map[string]string{
		claudeID:     realDocuments[claudeID],
		claudeCodeID: realDocuments[claudeCodeID],
		ipv6ID:       exampleDocument(ipv6ID, "http://[::1]/callback"),
	})
	for _, tc := range []struct {
		clientID, redirect string
		listed             bool
	}{
		{claudeCodeID, "http://localhost:53682/callback", true},
		{claudeCodeID, "http://127.0.0.1:1/callback", true},
		{claudeCodeID, "http://localhost/callback", true},
		{ipv6ID, "http://[::1]:53682/callback", true},
		{claudeCodeID, "http://localhost:53682/other", false},
		{claudeCodeID, "http://localhost:53682/callback?next=1", false},
		{claudeCodeID, "https://localhost:53682/callback", false},
		{claudeCodeID, "http://user@localhost:53682/callback", false},
		{claudeCodeID, "http://localhost:53682/callback#frag", false},
		{claudeCodeID, "http://localhost:53682/callback#", false},
		{claudeCodeID, "http://localhost:53682/callback?", false},
		{claudeCodeID, "http://[::1]:53682/callback", false},
		// Off loopback the port must match too.
		{claudeID, "https://claude.ai:8443/api/mcp/auth_callback", false},
	} {
		doc, err := c.Fetch(t.Context(), tc.clientID)
		if err != nil {
			t.Fatalf("Fetch(%s): %v", tc.clientID, err)
		}
		if doc.Lists(tc.redirect) != tc.listed {
			t.Errorf("%s: listed = %v, want %v", tc.redirect, !tc.listed, tc.listed)
		}
	}
}

func TestIsLoopback(t *testing.T) {
	for redirect, want := range map[string]bool{
		"http://localhost:53682/callback": true,
		"http://127.0.0.1/callback":       true,
		"http://[::1]:53682/callback":     true,
		"https://claude.ai/callback":      false,
		"http://localhost.example/cb":     false,
		"http://127.0.0.2/callback":       false,
		"http://user@localhost/callback":  false,
		"not a url":                       false,
	} {
		if got := IsLoopback(redirect); got != want {
			t.Errorf("IsLoopback(%q) = %v, want %v", redirect, got, want)
		}
	}
}
