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

// Mount installs the read-only /mcp handler on mux behind authFunc, wrapped in
// the Bearer-prv passthrough. It is the single definition of the /mcp middleware
// sandwich, shared by server.start and the test harness so the endpoint can't be
// reconstructed differently in tests than in production. authFunc MUST be
// private-key-only (rpc.WithPrivateKeyAuth): /mcp admits neither dashboard JWTs
// nor public keys.
func Mount(mux *http.ServeMux, handler http.Handler, authFunc authn.AuthFunc) {
	authMW := authn.NewMiddleware(authFunc)
	mux.Handle("/mcp", withAPIKeyPassthrough(authMW.Wrap(handler)))
}
