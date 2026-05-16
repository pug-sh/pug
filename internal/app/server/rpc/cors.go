package rpc

import (
	"log/slog"
	"net/http"

	connectcors "connectrpc.com/cors"
	"github.com/rs/cors"
)

func WithCORS(allowedOrigins []string, connectHandler http.Handler) http.Handler {
	allowCredentials := true
	for _, o := range allowedOrigins {
		if o == "*" {
			slog.Warn("CORS: using wildcard origin with credentials is insecure, set PUG_CORS_ORIGINS to specific origins in production")
			allowCredentials = false
			break
		}
	}

	c := cors.New(cors.Options{
		AllowCredentials: allowCredentials,
		AllowedHeaders: append(
			connectcors.AllowedHeaders(),
			"Authorization",
			"x-api-key",
			"X-Project-Id",
		),
		AllowedMethods: append(connectcors.AllowedMethods(), http.MethodOptions),
		AllowedOrigins: allowedOrigins,
		ExposedHeaders: connectcors.ExposedHeaders(),
		MaxAge:         7200,
	})
	return c.Handler(connectHandler)
}
