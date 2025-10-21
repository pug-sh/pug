package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	connectcors "connectrpc.com/cors"
	"connectrpc.com/grpcreflect"
	"github.com/fivebitsio/cotton/internal/gen/proto/auth/v1/authv1connect"
	"github.com/fivebitsio/cotton/internal/rpc/auth"
	"github.com/fivebitsio/cotton/pkg/logger"
	"github.com/joho/godotenv"
	"github.com/rs/cors"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

func main() {
	ctx, done := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer done()

	if err := godotenv.Load(); err != nil {
		logger.Log.Error("error loading .env file", slog.Any("err", err))
		os.Exit(1)
	}

	deps, err := newDependencies(ctx)
	if err != nil {
		logger.Log.Error("error while initializing dependencies", slog.Any("err", err))
		os.Exit(1)
	}

	defer deps.Close(ctx)

	authServer := auth.NewServer(deps.pgRo, deps.pgW, deps.jwtKey)

	// Register auth service
	authPath, authHandler := authv1connect.NewAuthServiceHandler(
		authServer,
		// Add any interceptors if needed
		// connect.WithInterceptors(yourInterceptor),
	)

	// Create a handler that combines auth and reflection
	handler := http.NewServeMux()
	handler.Handle(authPath, authHandler)

	// Optional: Add reflection for debugging/development
	services := []string{authv1connect.AuthServiceName}
	reflector := grpcreflect.NewStaticReflector(services...)
	handler.Handle(grpcreflect.NewHandlerV1(reflector))
	handler.Handle(grpcreflect.NewHandlerV1Alpha(reflector))

	// Wrap the handler with CORS middleware first
	handlerWithCORS := withCORS(handler)

	// Then wrap with h2c to support HTTP/2 and HTTP/1.1
	h2cHandler := h2c.NewHandler(handlerWithCORS, &http2.Server{})

	// Start the server
	logger.Log.Info("Starting server", slog.String("addr", ":8081"))
	if err := http.ListenAndServe(":8081", h2cHandler); err != nil {
		logger.Log.Error("failed to serve", slog.Any("err", err))
		os.Exit(1)
	}
}

// withCORS adds CORS support to a Connect HTTP handler.
func withCORS(connectHandler http.Handler) http.Handler {
	// TODO: Restrict CORS origins in production - only allowing all origins for development
	c := cors.New(cors.Options{
		AllowedOrigins:   []string{"*"},                                            // Allow all origins for development
		AllowedMethods:   append(connectcors.AllowedMethods(), http.MethodOptions), // Include OPTIONS method
		AllowedHeaders:   connectcors.AllowedHeaders(),
		ExposedHeaders:   connectcors.ExposedHeaders(),
		AllowCredentials: false, // Set to true if you need to send cookies/auth headers
		MaxAge:           7200,  // 2 hours in seconds
	})
	return c.Handler(connectHandler)
}
