package rpc

import (
	"log/slog"
	"net/http"

	connectcors "connectrpc.com/cors"
	"github.com/rs/cors"
)

func WithCORS(allowedOrigins []string, connectHandler http.Handler) http.Handler {
	for _, o := range allowedOrigins {
		if o == "*" {
			slog.Warn("CORS: using wildcard origin with credentials is insecure, set PUG_CORS_ORIGINS to specific origins in production")
			break
		}
	}

	c := cors.New(cors.Options{
		AllowCredentials: true,
		AllowedHeaders: append(
			connectcors.AllowedHeaders(),
			"Authorization",
			"X-Project-Id",
		),
		AllowedMethods: append(connectcors.AllowedMethods(), http.MethodOptions),
		AllowedOrigins: allowedOrigins,
		ExposedHeaders: connectcors.ExposedHeaders(),
		MaxAge:         7200,
	})
	return c.Handler(connectHandler)
}

// WithSDKCORS configures CORS for SDK endpoints: wildcard origin, no credentials.
// SDK auth is the x-api-key header (no cookies), and customer sites that embed
// the SDK have arbitrary origins the operator can't pre-list.
// TODO: replace the wildcard with a per-project allowed-origins list stored on
// the project record, so each customer can restrict origins for their key.
func WithSDKCORS(connectHandler http.Handler) http.Handler {
	c := cors.New(cors.Options{
		AllowCredentials: false,
		AllowedHeaders: append(
			connectcors.AllowedHeaders(),
			HeaderAPIKey,
		),
		AllowedMethods: connectcors.AllowedMethods(),
		AllowedOrigins: []string{"*"},
		ExposedHeaders: connectcors.ExposedHeaders(),
		MaxAge:         7200,
	})
	return c.Handler(connectHandler)
}
