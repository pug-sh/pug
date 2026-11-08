package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"connectrpc.com/authn"
	"connectrpc.com/connect"
	"github.com/pug-sh/pug/internal/app/server/rpc"
	"github.com/pug-sh/pug/internal/core/mcpgrants"
)

// credentialKey is the context key under which the edge middleware stashes the
// caller's credential so the loopback client can present it again on each
// in-process tool call.
type credentialKey struct{}

// credential is the header the caller authenticated with: a private key in
// x-api-key, or an MCP access token in Authorization.
type credential struct{ header, value string }

func withCredential(ctx context.Context, c credential) context.Context {
	return context.WithValue(ctx, credentialKey{}, c)
}

func credentialFrom(ctx context.Context) (credential, bool) {
	c, ok := ctx.Value(credentialKey{}).(credential)
	return c, ok
}

// withCredentialPassthrough wraps authenticate: it moves a `Bearer prv_` (all many
// clients can send) into the x-api-key WithMCPAuth reads, and stashes the key, or
// else an MCP access token, for the loopback. x-api-key wins, as in WithMCPAuth.
func withCredentialPassthrough(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if key := rpc.NormalizeBearerAPIKey(req); key != "" {
			req = req.WithContext(withCredential(req.Context(), credential{rpc.HeaderAPIKey, key}))
		} else if _, ok := rpc.MCPAccessToken(req); ok {
			req = req.WithContext(withCredential(req.Context(), credential{"Authorization", req.Header.Get("Authorization")}))
		}
		next.ServeHTTP(w, req)
	})
}

// authenticate is authn.Middleware's Wrap plus, with OAuth on (issuer set), a 401
// that points at the protected resource metadata: clients start signing in only on
// a 401 that carries it.
func authenticate(check authn.AuthFunc, issuer string, next http.Handler) http.Handler {
	errW := connect.NewErrorWriter()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info, err := check(r.Context(), r)
		if err != nil {
			if issuer != "" && connect.CodeOf(err) == connect.CodeUnauthenticated {
				w.Header().Set("WWW-Authenticate", challenge(r, issuer))
			}
			_ = errW.Write(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(authn.SetInfo(r.Context(), info)))
	})
}

// challenge names the metadata for the URL as the client was given it, with or
// without the slash, and error only when a credential was sent and refused.
func challenge(r *http.Request, issuer string) string {
	metadata := issuer + protectedResourcePath + "/mcp"
	if r.URL.Path != "/mcp" {
		metadata += "/"
	}
	params := fmt.Sprintf("resource_metadata=%q, scope=%q", metadata, mcpgrants.Scope)
	if r.Header.Get(rpc.HeaderAPIKey) != "" || r.Header.Get("Authorization") != "" {
		params = `error="invalid_token", ` + params
	}
	return "Bearer " + params
}

// OAuth turns on signing in to /mcp. Its zero value leaves OAuth off, and /mcp on
// private keys only.
type OAuth struct {
	Grants *mcpgrants.Service
	// AuthorizationEndpoint is the dashboard's consent page.
	AuthorizationEndpoint string
}

// Mount installs the read-only /mcp endpoint on mux. It is the single entry point
// for the whole subsystem — it builds the tool handler, constructs the auth
// boundary, and assembles the middleware sandwich — and server.start and the test
// harness both go through it, so the endpoint cannot be wired one way in
// production and another in tests.
//
// loopback is the mux each tool call is replayed through in-process (in practice
// the same mux being mounted on).
//
// Auth is built here, not passed in, so /mcp takes only a private key or, with
// OAuth on, an MCP access token, never a dashboard JWT or a public key. With OAuth
// on, Mount also serves the OAuth metadata and the token endpoint.
//
// It returns an error if the generated tool set has drifted from the curated
// policy table, so a codegen change that adds or renames a shared RPC fails server
// startup rather than silently shipping an unnamed or missing tool — the same
// fail-fast culture as the authz registry.
//
// Both "/mcp" and "/mcp/" are registered: Go 1.22's ServeMux exact-matches a
// pattern with no trailing slash and only redirects the other way, so a client
// configured with a trailing slash (or an ingress that appends one) would
// otherwise get an opaque 404.
func Mount(mux *http.ServeMux, loopback http.Handler, repo rpc.ProjectKeyLookup, oauth OAuth) error {
	if (oauth.Grants == nil) != (oauth.AuthorizationEndpoint == "") {
		return errors.New("mcp: OAuth needs both Grants and AuthorizationEndpoint, or neither")
	}
	handler, err := newHandler(loopback)
	if err != nil {
		return err
	}

	var issuer string
	if oauth.Grants != nil {
		issuer = oauth.Grants.Issuer()
		mountOAuth(mux, oauth)
	}
	mounted := withCredentialPassthrough(authenticate(rpc.WithMCPAuth(repo, oauth.Grants), issuer, handler))
	mux.Handle("/mcp", mounted)
	mux.Handle("/mcp/", mounted)

	return nil
}
