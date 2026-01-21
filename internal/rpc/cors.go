package rpc

import (
	"net/http"

	connectcors "connectrpc.com/cors"
	"github.com/fivebitsio/cotton/pkg/constant"
	"github.com/rs/cors"
)

func WithCORS(connectHandler http.Handler) http.Handler {
	// todo- replace wildcard origin with specific allowed origins in production.
	// using "*" with allowcredentials is insecure and allows csrf attacks.
	c := cors.New(cors.Options{
		AllowedOrigins: []string{"*"},
		AllowedMethods: append(connectcors.AllowedMethods(), http.MethodOptions),
		AllowedHeaders: append(
			connectcors.AllowedHeaders(),
			constant.HeaderAuthorization,
		),
		ExposedHeaders:   connectcors.ExposedHeaders(),
		AllowCredentials: true,
		MaxAge:           7200,
	})
	return c.Handler(connectHandler)
}
