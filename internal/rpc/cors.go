package rpc

import (
	"log/slog"
	"net/http"

	connectcors "connectrpc.com/cors"
	"github.com/fivebitsio/cotton/pkg/constant"
	"github.com/rs/cors"
)

func WithCORS(allowedOrigins []string, connectHandler http.Handler) http.Handler {
	for _, o := range allowedOrigins {
		if o == "*" {
			slog.Warn("CORS: using wildcard origin with credentials is insecure, set COTTON_CORS_ORIGINS to specific origins in production")
			break
		}
	}

	c := cors.New(cors.Options{
		AllowCredentials: true,
		AllowedHeaders: append(
			connectcors.AllowedHeaders(),
			constant.HeaderAuthorization,
		),
		AllowedMethods: append(connectcors.AllowedMethods(), http.MethodOptions),
		AllowedOrigins: allowedOrigins,
		ExposedHeaders: connectcors.ExposedHeaders(),
		MaxAge:         7200,
	})
	return c.Handler(connectHandler)
}
