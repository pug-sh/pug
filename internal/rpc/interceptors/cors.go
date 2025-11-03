package interceptors

import (
	"net/http"

	connectcors "connectrpc.com/cors"
	"github.com/rs/cors"
)

// WithCORS wraps the given HTTP handler with CORS middleware.
func WithCORS(connectHandler http.Handler) http.Handler {
	c := cors.New(cors.Options{
		AllowedOrigins:   []string{"https://*.garib.dev", "https://localhost:5173", "http://localhost:5173"},
		AllowCredentials: true,
		AllowedMethods:   connectcors.AllowedMethods(),
		AllowedHeaders:   []string{"*"}, // not working with connectcors.AllowedHeaders() and AllowCredentials set to true
		ExposedHeaders:   connectcors.ExposedHeaders(),
		MaxAge:           7200,
	})
	return c.Handler(connectHandler)
}
