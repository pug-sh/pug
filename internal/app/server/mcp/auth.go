package mcp

import (
	"context"
	"net/http"

	"connectrpc.com/authn"
	"github.com/pug-sh/pug/internal/app/server/rpc"
)

// apiKeyCtxKey is the context key under which the edge middleware stashes the
// caller's private API key so the loopback client can re-inject it as an
// x-api-key header on each in-process tool call.
type apiKeyCtxKey struct{}

func withAPIKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, apiKeyCtxKey{}, key)
}

func apiKeyFromContext(ctx context.Context) string {
	key, _ := ctx.Value(apiKeyCtxKey{}).(string)
	return key
}

// withAPIKeyPassthrough is the outermost /mcp middleware. It normalises a
// `Authorization: Bearer prv_...` credential (the only header form many MCP
// clients can send) into the x-api-key header that the WithPrivateKeyAuth authn
// func expects — via rpc.NormalizeBearerAPIKey, which owns the Bearer scheme and
// private-key vocabulary — and stashes the resolved private key in the request
// context for the loopback client to re-inject on each inner tool call.
//
// A request that already carries x-api-key is left untouched but still has its
// key stashed. A non-`prv_` bearer (or no credential at all) is passed through
// unchanged, so the downstream authn boundary rejects it with a 401.
func withAPIKeyPassthrough(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if key := rpc.NormalizeBearerAPIKey(req); key != "" {
			req = req.WithContext(withAPIKey(req.Context(), key))
		}
		next.ServeHTTP(w, req)
	})
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
// Auth is private-key-only and is built here from repo rather than accepted as a
// parameter, so it holds BY CONSTRUCTION: there is no wiring in which /mcp admits
// a dashboard JWT (MCP clients hold a static credential, so an expiring access
// token is useless there, and a JWT would widen the endpoint from project- to
// customer-scoped) or a public key (extractable from client apps). An earlier
// shape took an authn.AuthFunc, which let the test harness pass its own — so the
// suite stayed green even when the endpoint was pointed at WithDualAuth.
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
func Mount(mux *http.ServeMux, loopback http.Handler, repo rpc.ProjectKeyLookup) error {
	handler, err := newHandler(loopback)
	if err != nil {
		return err
	}

	authMW := authn.NewMiddleware(rpc.WithPrivateKeyAuth(repo))
	mounted := withAPIKeyPassthrough(authMW.Wrap(handler))
	mux.Handle("/mcp", mounted)
	mux.Handle("/mcp/", mounted)

	return nil
}
