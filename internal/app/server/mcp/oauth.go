package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"go.opentelemetry.io/otel"

	"github.com/pug-sh/pug/internal/core/mcpgrants"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/slogx"
)

const (
	protectedResourcePath = "/.well-known/oauth-protected-resource"
	// A token request is a few short form fields.
	maxTokenRequestBytes = 16 << 10
)

// authServerMetadata is RFC 8414's document. oauthex.AuthServerMeta would write an
// empty jwks_uri, which strict clients reject.
type authServerMetadata struct {
	Issuer                                     string   `json:"issuer"`
	AuthorizationEndpoint                      string   `json:"authorization_endpoint"`
	TokenEndpoint                              string   `json:"token_endpoint"`
	ScopesSupported                            []string `json:"scopes_supported"`
	ResponseTypesSupported                     []string `json:"response_types_supported"`
	GrantTypesSupported                        []string `json:"grant_types_supported"`
	CodeChallengeMethodsSupported              []string `json:"code_challenge_methods_supported"`
	TokenEndpointAuthMethodsSupported          []string `json:"token_endpoint_auth_methods_supported"`
	ClientIDMetadataDocumentSupported          bool     `json:"client_id_metadata_document_supported"`
	AuthorizationResponseIssParameterSupported bool     `json:"authorization_response_iss_parameter_supported"`
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

// mountOAuth serves the metadata a client discovers sign-in from, and the token
// endpoint. The authorization endpoint is the dashboard's consent page.
func mountOAuth(mux *http.ServeMux, oauth OAuth) {
	issuer := oauth.Grants.Issuer()
	resource := func(path string) http.Handler {
		return auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
			Resource:               issuer + path,
			AuthorizationServers:   []string{issuer},
			ScopesSupported:        []string{mcpgrants.Scope},
			BearerMethodsSupported: []string{"header"},
			ResourceName:           serverName,
		})
	}
	mux.Handle(protectedResourcePath, resource("/mcp"))
	mux.Handle(protectedResourcePath+"/mcp", resource("/mcp"))
	// Claude compares resource with the URL as the person typed it.
	mux.Handle(protectedResourcePath+"/mcp/{$}", resource("/mcp/"))

	metadata := authServerMetadata{
		Issuer:                                     issuer,
		AuthorizationEndpoint:                      oauth.AuthorizationEndpoint,
		TokenEndpoint:                              issuer + "/oauth/token",
		ScopesSupported:                            []string{mcpgrants.Scope},
		ResponseTypesSupported:                     []string{"code"},
		GrantTypesSupported:                        []string{"authorization_code", "refresh_token"},
		CodeChallengeMethodsSupported:              []string{"S256"},
		TokenEndpointAuthMethodsSupported:          []string{"none"},
		ClientIDMetadataDocumentSupported:          true,
		AuthorizationResponseIssParameterSupported: true,
	}
	mux.Handle("/.well-known/oauth-authorization-server", anyOrigin(http.MethodGet, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, metadata)
	}))
	mux.Handle("/oauth/token", anyOrigin(http.MethodPost, tokenEndpoint{grants: oauth.Grants}.serve))
}

// anyOrigin serves h for method and answers CORS preflights. Browser-based MCP
// clients read these answers, and no cookie is involved, so any origin may.
func anyOrigin(method string, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		switch r.Method {
		case method:
			h(w, r)
		case http.MethodOptions:
			w.Header().Set("Access-Control-Allow-Methods", method)
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.WriteHeader(http.StatusNoContent)
		default:
			w.Header().Set("Allow", method+", "+http.MethodOptions)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
}

type tokenEndpoint struct{ grants *mcpgrants.Service }

// serve exchanges a code or a refresh token, answering as RFC 6749 section 5 does.
func (t tokenEndpoint) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	// Nothing upstream starts a span, and the service records its failures on one.
	ctx, span := otel.Tracer("server/mcp").Start(r.Context(), "mcp.oauth.token")
	defer span.End()
	defer func() {
		if v := recover(); v != nil {
			ctx := context.WithoutCancel(ctx)
			err := fmt.Errorf("mcp token endpoint: panic: %v", v)
			slog.ErrorContext(ctx, "panic serving an mcp token request", slogx.Error(err),
				slog.String("stack", string(debug.Stack())))
			telemetry.RecordError(ctx, err)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}()

	r.Body = http.MaxBytesReader(w, r.Body, maxTokenRequestBytes)
	if err := r.ParseForm(); err != nil {
		slog.WarnContext(ctx, "refused an unreadable mcp token request", slogx.Error(err))
		writeOAuthError(w, http.StatusBadRequest, mcpgrants.ErrInvalidRequest)
		return
	}
	form := r.PostForm // never the query string
	// Refused before the code is read: Go's oauth2 tries Basic first, then sends the
	// same code again in the form, so a refused header is routine.
	if r.Header.Get("Authorization") != "" || form.Has("client_secret") {
		if form.Has("client_secret") {
			slog.WarnContext(ctx, "refused a client secret at the mcp token endpoint", slog.String("client_id", form.Get("client_id")))
		} else {
			slog.DebugContext(ctx, "refused an authorization header at the mcp token endpoint")
		}
		w.Header().Set("WWW-Authenticate", "Basic")
		writeOAuthError(w, http.StatusUnauthorized, mcpgrants.ErrInvalidClient)
		return
	}

	var tokens mcpgrants.Tokens
	var err error
	switch form.Get("grant_type") {
	case "authorization_code":
		tokens, err = t.grants.ExchangeCode(ctx, mcpgrants.CodeExchange{
			ClientID:     form.Get("client_id"),
			Code:         form.Get("code"),
			CodeVerifier: form.Get("code_verifier"),
			RedirectURI:  form.Get("redirect_uri"),
			Resource:     form.Get("resource"),
		})
	case "refresh_token":
		tokens, err = t.grants.Refresh(ctx, mcpgrants.TokenRefresh{
			ClientID:     form.Get("client_id"),
			RefreshToken: form.Get("refresh_token"),
			Resource:     form.Get("resource"),
		})
	case "":
		err = mcpgrants.ErrInvalidRequest
	default:
		err = mcpgrants.ErrUnsupportedGrantType
	}
	if oauthErr, ok := errors.AsType[*mcpgrants.Error](err); ok {
		slog.WarnContext(ctx, "refused an mcp token request", slogx.Error(err),
			slog.String("grant_type", form.Get("grant_type")), slog.String("client_id", form.Get("client_id")),
			slog.String("content_type", r.Header.Get("Content-Type")))
		writeOAuthError(w, http.StatusBadRequest, oauthErr)
		return
	}
	if err != nil {
		// Disposition only: the service logged and recorded it. A 503 has the client
		// retry rather than drop the connection.
		slog.WarnContext(ctx, "mcp token request failed", slogx.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "temporarily_unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, tokenResponse{
		AccessToken:  tokens.AccessToken,
		TokenType:    "Bearer",
		ExpiresIn:    int(mcpgrants.AccessTokenTTL.Seconds()),
		RefreshToken: tokens.RefreshToken,
		Scope:        mcpgrants.Scope,
	})
}

func writeOAuthError(w http.ResponseWriter, status int, err *mcpgrants.Error) {
	writeJSON(w, status, map[string]string{"error": err.Code()})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
